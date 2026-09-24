package liverunner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMediaMTXConfigIsMinimalLowLatencyAndPrivate(t *testing.T) {
	config, err := RenderMediaMTXConfigV1(DefaultMediaMTXConfigV1("http://127.0.0.1:8080/internal/mediamtx/auth"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"authMethod: http", "hlsVariant: lowLatency", "hlsAlwaysRemux: true",
		"rtmp: true", "rtsp: false", "webrtc: false", "srt: false", "moq: false",
		"hlsAddress: 127.0.0.1:8888", "apiAddress: 127.0.0.1:9997", "metricsAddress: 127.0.0.1:9998",
		"overridePublisher: false", "record: false",
	} {
		if !bytes.Contains(config, []byte(required)) {
			t.Fatalf("generated config missing %q:\n%s", required, config)
		}
	}
	if bytes.Contains(config, []byte("internal-router-secret")) {
		t.Fatal("generated MediaMTX config contains the runner media token")
	}
	unsafe := DefaultMediaMTXConfigV1("http://127.0.0.1:8080/internal/mediamtx/auth")
	unsafe.HLSAddress = ":8888"
	if _, err := RenderMediaMTXConfigV1(unsafe); err == nil {
		t.Fatal("public MediaMTX HLS listener was accepted")
	}
	unsafe.HLSAddress = "127.0.0.1:8888\nhlsEncryption: true"
	if _, err := RenderMediaMTXConfigV1(unsafe); err == nil {
		t.Fatal("configuration injection through a listen address was accepted")
	}
}

func TestMediaMTXAuthorizationSeparatesIngestInternalAndPlayback(t *testing.T) {
	store, request, response, streamKey := mediaTestSessionV1(t)
	rootToken := "0123456789abcdef0123456789abcdef"
	authorizer := MediaMTXAuthorizerV1{Sessions: store, InternalTokenRoot: rootToken}
	internalToken := InternalMediaTokenV1(rootToken, response.RunnerSessionID)
	ingestPath, _ := IngestMediaPathV1(response.RunnerSessionID)
	renditionPath, _ := RenditionMediaPathV1(response.RunnerSessionID, "720p")

	tests := []struct {
		name    string
		request MediaMTXAuthRequestV1
		want    bool
	}{
		{"gateway publish", MediaMTXAuthRequestV1{Action: "publish", Path: ingestPath, Protocol: "rtmp", Token: streamKey}, true},
		{"wrong gateway key", MediaMTXAuthRequestV1{Action: "publish", Path: ingestPath, Protocol: "rtmp", Token: "wrong"}, false},
		{"runner ingest read", MediaMTXAuthRequestV1{Action: "read", Path: ingestPath, Protocol: "rtmp", Token: internalToken}, true},
		{"customer cannot read ingest", MediaMTXAuthRequestV1{Action: "read", Path: ingestPath, Protocol: "rtmp", Token: streamKey}, false},
		{"runner rendition publish", MediaMTXAuthRequestV1{Action: "publish", Path: renditionPath, Protocol: "rtmp", Token: internalToken}, true},
		{"customer cannot publish rendition", MediaMTXAuthRequestV1{Action: "publish", Path: renditionPath, Protocol: "rtmp", Token: streamKey}, false},
		{"private HLS playback", MediaMTXAuthRequestV1{Action: "read", Path: renditionPath, Protocol: "hls"}, true},
		{"unknown path", MediaMTXAuthRequestV1{Action: "read", Path: "other/" + response.RunnerSessionID, Protocol: "hls"}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := authorizer.Authorize(test.request); got != test.want {
				t.Fatalf("Authorize(%+v)=%v want %v", test.request, got, test.want)
			}
		})
	}

	ended := testEventV1(response.RunnerSessionID, 1, "session.ended", "ended", 0, "gateway_close")
	if err := store.Advance(request.SessionID, ended); err != nil {
		t.Fatal(err)
	}
	if authorizer.Authorize(MediaMTXAuthRequestV1{Action: "read", Path: renditionPath, Protocol: "hls"}) {
		t.Fatal("terminal session retained media access")
	}
}

func TestMediaMTXAuthorizationTracksKeyRotation(t *testing.T) {
	store, request, response, oldKey := mediaTestSessionV1(t)
	authorizer := MediaMTXAuthorizerV1{Sessions: store, InternalTokenRoot: "0123456789abcdef0123456789abcdef"}
	ingestPath, _ := IngestMediaPathV1(response.RunnerSessionID)
	rotation := StreamKeyIssueRequestV1{RequestID: "key_issue_002", Audience: "gateway-relay"}
	rotatedKey, _ := BuildPrivateIngestStreamKeyV1(response.RunnerSessionID, "new-private-ingest-key")
	rotated := StreamKeyIssueResponseV1{RequestID: rotation.RequestID, StreamKey: rotatedKey, ExpiresAt: "2030-01-01T00:00:00Z"}
	if _, _, err := store.RecordKeyIssue(request.SessionID, rotation, rotated); err != nil {
		t.Fatal(err)
	}
	if authorizer.Authorize(MediaMTXAuthRequestV1{Action: "publish", Path: ingestPath, Protocol: "rtmp", Token: oldKey}) {
		t.Fatal("superseded stream key remained authorized")
	}
	if authorizer.Authorize(MediaMTXAuthRequestV1{Action: "publish", Path: ingestPath, Protocol: "rtmp", Token: "new-private-ingest-key"}) {
		t.Fatal("replacement stream key was authorized before publisher kick completed")
	}
	internalToken := InternalMediaTokenV1(authorizer.InternalTokenRoot, response.RunnerSessionID)
	if !authorizer.Authorize(MediaMTXAuthRequestV1{Action: "read", Path: ingestPath, Protocol: "rtmp", Token: internalToken}) {
		t.Fatal("key activation interrupted the runner's existing ingest read")
	}
	renditionPath, _ := RenditionMediaPathV1(response.RunnerSessionID, "720p")
	if !authorizer.Authorize(MediaMTXAuthRequestV1{Action: "read", Path: renditionPath, Protocol: "hls"}) {
		t.Fatal("key activation interrupted HLS playback")
	}
	if err := store.CompleteKeyActivation(request.SessionID, rotation.RequestID); err != nil {
		t.Fatal(err)
	}
	if !authorizer.Authorize(MediaMTXAuthRequestV1{Action: "publish", Path: ingestPath, Protocol: "rtmp", Token: "new-private-ingest-key"}) {
		t.Fatal("rotated stream key was not authorized")
	}
}

func TestMediaMTXAuthorizationRejectsExpiredIngestKey(t *testing.T) {
	store, _, response, token := mediaTestSessionV1(t)
	path, _ := IngestMediaPathV1(response.RunnerSessionID)
	authorizer := MediaMTXAuthorizerV1{
		Sessions: store, InternalTokenRoot: "0123456789abcdef0123456789abcdef",
		Now: func() time.Time { return time.Date(2030, 1, 1, 0, 0, 1, 0, time.UTC) },
	}
	if authorizer.Authorize(MediaMTXAuthRequestV1{Action: "publish", Path: path, Protocol: "rtmp", Token: token}) {
		t.Fatal("expired private ingest key remained authorized")
	}
}

func TestMediaMTXAuthHTTPHandlerFailsClosed(t *testing.T) {
	store, _, response, streamKey := mediaTestSessionV1(t)
	handler := MediaMTXAuthorizerV1{Sessions: store, InternalTokenRoot: "0123456789abcdef0123456789abcdef"}
	ingestPath, _ := IngestMediaPathV1(response.RunnerSessionID)
	body, _ := json.Marshal(MediaMTXAuthRequestV1{Action: "publish", Path: ingestPath, Protocol: "rtmp", Token: streamKey})

	request := httptest.NewRequest(http.MethodPost, "/internal/mediamtx/auth", bytes.NewReader(body))
	responseRecorder := httptest.NewRecorder()
	handler.ServeHTTP(responseRecorder, request)
	if responseRecorder.Code != http.StatusNoContent || responseRecorder.Body.Len() != 0 {
		t.Fatalf("authorized response=%d %q", responseRecorder.Code, responseRecorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/internal/mediamtx/auth", strings.NewReader(`{"action":"publish","path":"bad","protocol":"rtmp","token":"do-not-echo","extra":true}`))
	responseRecorder = httptest.NewRecorder()
	handler.ServeHTTP(responseRecorder, request)
	if responseRecorder.Code != http.StatusBadRequest || strings.Contains(responseRecorder.Body.String(), "do-not-echo") {
		t.Fatalf("malformed response=%d %q", responseRecorder.Code, responseRecorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/internal/mediamtx/auth", strings.NewReader(`{"userAgent":"`+strings.Repeat("x", 17*1024)+`"}`))
	responseRecorder = httptest.NewRecorder()
	handler.ServeHTTP(responseRecorder, request)
	if responseRecorder.Code != http.StatusBadRequest {
		t.Fatalf("oversized authentication request response=%d", responseRecorder.Code)
	}
}

func TestMediaPathsRejectTraversal(t *testing.T) {
	for _, raw := range []string{"/ingest/runner", "ingest/../runner", "renditions/runner", "renditions/runner/../../secret"} {
		if _, _, _, ok := parseMediaPathV1(raw); ok {
			t.Fatalf("unsafe media path accepted: %q", raw)
		}
	}
}

func TestInternalMediaTokensAreStableAndSessionScoped(t *testing.T) {
	root := "0123456789abcdef0123456789abcdef"
	first := InternalMediaTokenV1(root, "runner_session_001")
	if first != InternalMediaTokenV1(root, "runner_session_001") || first == InternalMediaTokenV1(root, "runner_session_002") || len(first) != 64 {
		t.Fatal("internal media token derivation is not stable and session-scoped")
	}
}

func TestPrivateIngestStreamKeyComposesPathAndEscapedToken(t *testing.T) {
	streamKey, err := BuildPrivateIngestStreamKeyV1("runner_session_001", "private key+rotation")
	if err != nil {
		t.Fatal(err)
	}
	streamPath, token, ok := ParsePrivateIngestStreamKeyV1(streamKey)
	if !ok || streamPath != "ingest/runner_session_001" || token != "private key+rotation" {
		t.Fatalf("stream key parsed as path=%q token=%q valid=%v", streamPath, token, ok)
	}
	for _, invalid := range []string{"runner_session_001", "runner_session_001?token=", "runner_session_001?token=one&token=two", "ingest/runner_session_001?token=secret"} {
		if _, _, ok := ParsePrivateIngestStreamKeyV1(invalid); ok {
			t.Fatalf("invalid stream key accepted: %q", invalid)
		}
	}
}

func TestMediaMTXImageAcceptsGeneratedConfig(t *testing.T) {
	if os.Getenv("LIVE_RUNNER_CONTAINER_TEST") != "1" {
		t.Skip("set LIVE_RUNNER_CONTAINER_TEST=1 to exercise the pinned MediaMTX image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	config, err := RenderMediaMTXConfigV1(DefaultMediaMTXConfigV1("http://127.0.0.1:8080/internal/mediamtx/auth"))
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "mediamtx.yml")
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	containerName := fmt.Sprintf("live-runner-mediamtx-test-%d", os.Getpid())
	cleanup := func() { _ = exec.Command("docker", "rm", "-f", containerName).Run() }
	cleanup()
	defer cleanup()
	command := exec.CommandContext(ctx, "docker", "run", "--rm", "--name", containerName, "-v", configPath+":/mediamtx.yml:ro", MediaMTXImageV1)
	output, runErr := command.CombinedOutput()
	cleanup()
	if ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("MediaMTX exited before readiness window: err=%v output=%s", runErr, output)
	}
	if bytes.Contains(bytes.ToLower(output), []byte("error")) {
		t.Fatalf("MediaMTX reported a configuration error: %s", output)
	}
}

func TestMediaMTXRealPublisherCanBeKicked(t *testing.T) {
	if os.Getenv("LIVE_RUNNER_CONTAINER_TEST") != "1" {
		t.Skip("set LIVE_RUNNER_CONTAINER_TEST=1 to exercise real MediaMTX RTMP")
	}
	for _, binary := range []string{"docker", "ffmpeg"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skip(binary + " is not installed")
		}
	}
	store, _, createResponse, streamToken := mediaTestSessionV1(t)
	authServer := httptest.NewServer(MediaMTXAuthorizerV1{Sessions: store, InternalTokenRoot: "0123456789abcdef0123456789abcdef"})
	defer authServer.Close()
	rtmpPort := freeTCPPortV1(t)
	hlsPort := freeTCPPortV1(t)
	apiPort := freeTCPPortV1(t)
	metricsPort := freeTCPPortV1(t)
	configValue := DefaultMediaMTXConfigV1(authServer.URL)
	configValue.RTMPAddress = fmt.Sprintf("127.0.0.1:%d", rtmpPort)
	configValue.HLSAddress = fmt.Sprintf("127.0.0.1:%d", hlsPort)
	configValue.APIAddress = fmt.Sprintf("127.0.0.1:%d", apiPort)
	configValue.MetricsAddress = fmt.Sprintf("127.0.0.1:%d", metricsPort)
	config, err := RenderMediaMTXConfigV1(configValue)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "mediamtx.yml")
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	containerName := fmt.Sprintf("live-runner-mediamtx-real-%d", os.Getpid())
	cleanup := func() { _ = exec.Command("docker", "rm", "-f", containerName).Run() }
	cleanup()
	defer cleanup()
	routerProcess := exec.Command("docker", "run", "--rm", "--network", "host", "--name", containerName, "-v", configPath+":/mediamtx.yml:ro", MediaMTXImageV1)
	routerProcess.Stdout = io.Discard
	routerProcess.Stderr = io.Discard
	if err := routerProcess.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup()
		_ = routerProcess.Wait()
	}()
	apiBase := fmt.Sprintf("http://127.0.0.1:%d", apiPort)
	waitForHTTPV1(t, apiBase+"/v3/paths/list", 5*time.Second)

	streamKey, err := BuildPrivateIngestStreamKeyV1(createResponse.RunnerSessionID, streamToken)
	if err != nil {
		t.Fatal(err)
	}
	publishURL := fmt.Sprintf("rtmp://127.0.0.1:%d/ingest/%s", rtmpPort, streamKey)
	publisher := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-re", "-f", "lavfi", "-i", "testsrc=size=160x90:rate=10", "-re", "-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=44100", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-pix_fmt", "yuv420p", "-c:a", "aac", "-f", "flv", publishURL)
	publisher.Stdout = io.Discard
	publisher.Stderr = io.Discard
	if err := publisher.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if publisher.Process != nil {
			_ = publisher.Process.Kill()
		}
	}()
	routerClient, err := NewMediaRouterClientV1(apiBase, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ingestPath, _ := IngestMediaPathV1(createResponse.RunnerSessionID)
	waitContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := routerClient.WaitForRTMPPublisher(waitContext, ingestPath, 20*time.Millisecond); err != nil {
		t.Fatalf("real RTMP publisher did not come online: %v", err)
	}
	if err := routerClient.KickPublisher(context.Background(), ingestPath); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- publisher.Wait() }()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("FFmpeg publisher remained connected after MediaMTX kick")
	}
	cleanup()
}

func freeTCPPortV1(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func waitForHTTPV1(t *testing.T, endpoint string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	for time.Now().Before(deadline) {
		response, err := client.Get(endpoint)
		if err == nil {
			response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 500 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("HTTP endpoint did not become ready: %s", endpoint)
}

func mediaTestSessionV1(t *testing.T) (*EncryptedFileSessionStoreV1, RunnerCreateRequestV1, RunnerCreateResponseV1, string) {
	t.Helper()
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x44}, 32))
	request, response := testCreatePairV1(t)
	if _, _, _, err := store.CreateOrReplay(request, response); err != nil {
		t.Fatal(err)
	}
	issue := readStrictFixtureV1[StreamKeyIssueRequestV1](t, "key-issue-request.json")
	issued := readStrictFixtureV1[StreamKeyIssueResponseV1](t, "key-issue-response.json")
	issued.StreamKey, _ = BuildPrivateIngestStreamKeyV1(response.RunnerSessionID, issued.StreamKey)
	if _, _, err := store.RecordKeyIssue(request.SessionID, issue, issued); err != nil {
		t.Fatal(err)
	}
	_, token, _ := ParsePrivateIngestStreamKeyV1(issued.StreamKey)
	return store, request, response, token
}
