package liverunner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLiveHLSRealMediaMTXPlaylistIsPubliclyReachable(t *testing.T) {
	if os.Getenv("LIVE_RUNNER_CONTAINER_TEST") != "1" {
		t.Skip("set LIVE_RUNNER_CONTAINER_TEST=1 to exercise real MediaMTX HLS")
	}
	for _, binary := range []string{"docker", "ffmpeg"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skip(binary + " is not installed")
		}
	}
	store, request, response, _ := mediaTestSessionV1(t)
	if err := store.Advance(request.SessionID, testEventV1(response.RunnerSessionID, 1, "session.started", "active", 0, "")); err != nil {
		t.Fatal(err)
	}
	internalRoot := "0123456789abcdef0123456789abcdef"
	authorizer := MediaMTXAuthorizerV1{Sessions: store, InternalTokenRoot: internalRoot}
	var authMu sync.Mutex
	var authRequests []MediaMTXAuthRequestV1
	auth := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var value MediaMTXAuthRequestV1
		if err := json.NewDecoder(request.Body).Decode(&value); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		authMu.Lock()
		authRequests = append(authRequests, value)
		authMu.Unlock()
		if !authorizer.Authorize(value) {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer auth.Close()
	rtmpPort, hlsPort, apiPort, metricsPort := freeTCPPortV1(t), freeTCPPortV1(t), freeTCPPortV1(t), freeTCPPortV1(t)
	config := DefaultMediaMTXConfigV1(auth.URL)
	config.RTMPAddress = fmt.Sprintf("127.0.0.1:%d", rtmpPort)
	config.HLSAddress = fmt.Sprintf("127.0.0.1:%d", hlsPort)
	config.APIAddress = fmt.Sprintf("127.0.0.1:%d", apiPort)
	config.MetricsAddress = fmt.Sprintf("127.0.0.1:%d", metricsPort)
	body, err := RenderMediaMTXConfigV1(config)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "mediamtx.yml")
	if err := os.WriteFile(configPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	containerName := fmt.Sprintf("live-runner-hls-real-%d", os.Getpid())
	cleanupRouter := func() { _ = exec.Command("docker", "rm", "-f", containerName).Run() }
	cleanupRouter()
	defer cleanupRouter()
	router := exec.Command("docker", "run", "--rm", "--network", "host", "--name", containerName, "-v", configPath+":/mediamtx.yml:ro", MediaMTXImageV1)
	router.Stdout, router.Stderr = io.Discard, io.Discard
	if err := router.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cleanupRouter(); _ = router.Wait() }()
	waitForHTTPV1(t, fmt.Sprintf("http://127.0.0.1:%d/v3/paths/list", apiPort), 5*time.Second)

	internalToken := InternalMediaTokenV1(internalRoot, response.RunnerSessionID)
	publishURL := fmt.Sprintf("rtmp://127.0.0.1:%d/renditions/%s/720p?token=%s", rtmpPort, response.RunnerSessionID, internalToken)
	publisher := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-re", "-f", "lavfi", "-i", "testsrc=size=160x90:rate=10", "-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=44100", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-force_key_frames", "expr:gte(t,n_forced*1)", "-pix_fmt", "yuv420p", "-c:a", "aac", "-f", "flv", publishURL)
	publisher.Stdout, publisher.Stderr = io.Discard, io.Discard
	if err := publisher.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = publisher.Process.Kill(); _ = publisher.Wait() }()
	routerClient, err := NewMediaRouterClientV1(fmt.Sprintf("http://127.0.0.1:%d", apiPort), nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	renderPath, _ := RenditionMediaPathV1(response.RunnerSessionID, "720p")
	publishContext, cancelPublish := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPublish()
	if _, err := routerClient.WaitForRTMPPublisher(publishContext, renderPath, 20*time.Millisecond); err != nil {
		t.Fatalf("real rendition publisher did not come online: %v", err)
	}
	jar, _ := cookiejar.New(nil)
	directClient := &http.Client{Timeout: 10 * time.Second, Jar: jar}
	directResponse, directErr := directClient.Get(fmt.Sprintf("http://127.0.0.1:%d/%s/index.m3u8", hlsPort, renderPath))
	if directErr != nil {
		t.Fatalf("direct MediaMTX HLS request failed: %v", directErr)
	}
	directResponse.Body.Close()
	if directResponse.StatusCode != http.StatusOK {
		t.Fatalf("direct MediaMTX HLS status=%d", directResponse.StatusCode)
	}

	handler, err := NewHLSHandlerV1(store, testLivePresetsV1(), fmt.Sprintf("http://127.0.0.1:%d", hlsPort), nil, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	var playlist *httptest.ResponseRecorder
	for time.Now().Before(deadline) {
		playlist = hlsRequestV1(t, handler, response.RunnerSessionID, "720p/index.m3u8", "")
		if playlist.Code == http.StatusOK && strings.Contains(playlist.Body.String(), "video1_stream.m3u8") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if playlist == nil || playlist.Code != http.StatusOK || !strings.Contains(playlist.Body.String(), "video1_stream.m3u8") {
		t.Fatalf("real LL-HLS playlist status=%d body=%q", playlist.Code, playlist.Body.String())
	}
	parts := hlsRequestV1(t, handler, response.RunnerSessionID, "720p/video1_stream.m3u8", "")
	if parts.Code != http.StatusOK || !strings.Contains(parts.Body.String(), "#EXT-X-PART") {
		authMu.Lock()
		safeAuth := make([]string, 0, len(authRequests))
		for _, value := range authRequests {
			safeAuth = append(safeAuth, value.Protocol+":"+value.Action+":"+value.Path)
		}
		authMu.Unlock()
		handler.mu.RLock()
		cookieNames := make([]string, 0, len(handler.cookies[response.RunnerSessionID]))
		for _, cookie := range handler.cookies[response.RunnerSessionID] {
			cookieNames = append(cookieNames, cookie.Name+":"+cookie.Path)
		}
		handler.mu.RUnlock()
		t.Fatalf("real LL-HLS parts=%d %q auth=%v cookies=%v", parts.Code, parts.Body.String(), safeAuth, cookieNames)
	}
	master := hlsRequestV1(t, handler, response.RunnerSessionID, "master.m3u8", "")
	if master.Code != http.StatusOK || !strings.Contains(master.Body.String(), "720p/index.m3u8") {
		t.Fatalf("real HLS master=%d %q", master.Code, master.Body.String())
	}
	ready := make(chan struct{})
	close(ready)
	meter, err := NewLiveOutputMeterV1(store, handler, fakePublisherWaiterV1{ready: ready}, 50*time.Millisecond, time.Hour, 5*time.Second, 20*time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	usageDeadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(usageDeadline) {
		meter.poll(context.Background(), SessionRecordV1{BrokerSessionID: request.SessionID, RunnerSessionID: response.RunnerSessionID}, "ingest/"+response.RunnerSessionID, renderPath)
		metered, _, loadErr := store.Load(request.SessionID)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if metered.UsageTotal >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	metered, _, err := store.Load(request.SessionID)
	if err != nil || metered.UsageTotal < 1 || len(metered.MeteredSegmentSHA256) == 0 {
		t.Fatalf("real finalized HLS was not metered: usage=%d segments=%d err=%v", metered.UsageTotal, len(metered.MeteredSegmentSHA256), err)
	}
	cleanupRouter()
}

func TestHLSHandlerServesMasterAndStrictRenditionAssets(t *testing.T) {
	var upstreamRequest *http.Request
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamRequest = request.Clone(context.Background())
		if strings.HasSuffix(request.URL.Path, "/redirect.m3u8") {
			writer.Header().Set("Location", "https://attacker.example/playlist.m3u8")
			writer.WriteHeader(http.StatusFound)
			return
		}
		if strings.HasSuffix(request.URL.Path, "/video1_stream.m3u8") {
			writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = writer.Write([]byte("#EXTM3U\n#EXTINF:1.0,\nsegment.mp4\n"))
			return
		}
		if request.URL.RawQuery == "" && strings.HasSuffix(request.URL.Path, "/index.m3u8") {
			writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = writer.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nvideo1_stream.m3u8\n"))
			return
		}
		writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		writer.Header().Set("Set-Cookie", "secret=unsafe")
		writer.Header().Set("X-Upstream-Internal", "unsafe")
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write([]byte("#EXTM3U\n#EXT-X-PART:URI=\"part0.mp4\"\n"))
	}))
	defer upstream.Close()
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x7a}, 32))
	record, _ := createRuntimeSessionV1(t, store, "sess_hls_001", "runner_hls_001")
	handler, err := NewHLSHandlerV1(store, testLivePresetsV1(), upstream.URL, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	master := hlsRequestV1(t, handler, record.RunnerSessionID, "master.m3u8", "")
	if master.Code != http.StatusOK || master.Header().Get("Cache-Control") != "no-store" || !strings.Contains(master.Body.String(), "BANDWIDTH=3846000,RESOLUTION=1280x720,CODECS=\"avc1.64001f,mp4a.40.2\"") || !strings.Contains(master.Body.String(), "720p/index.m3u8") {
		t.Fatalf("master=%d headers=%v body=%q", master.Code, master.Header(), master.Body.String())
	}

	assetRequest := httptest.NewRequest(http.MethodGet, "/v1/public/sessions/"+record.RunnerSessionID+"/720p/index.m3u8?_HLS_msn=4&_HLS_part=2", nil)
	assetRequest.SetPathValue("id", record.RunnerSessionID)
	assetRequest.SetPathValue("asset", "720p/index.m3u8")
	assetRequest.Header.Set("Authorization", "Bearer must-not-forward")
	assetRequest.Header.Set("Cookie", "must-not-forward=1")
	assetRequest.Header.Set("Range", "bytes=0-99")
	asset := httptest.NewRecorder()
	handler.ServeHTTP(asset, assetRequest)
	if asset.Code != http.StatusPartialContent || asset.Body.String() == "" || asset.Header().Get("Set-Cookie") != "" || asset.Header().Get("X-Upstream-Internal") != "" || asset.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("asset=%d headers=%v body=%q", asset.Code, asset.Header(), asset.Body.String())
	}
	if upstreamRequest == nil || upstreamRequest.URL.Path != "/renditions/runner_hls_001/720p/index.m3u8" || upstreamRequest.URL.RawQuery != "_HLS_msn=4&_HLS_part=2" || upstreamRequest.Header.Get("Range") != "bytes=0-99" || upstreamRequest.Header.Get("Authorization") != "" || upstreamRequest.Header.Get("Cookie") != "" {
		t.Fatalf("upstream request=%+v", upstreamRequest)
	}
	redirect := hlsRequestV1(t, handler, record.RunnerSessionID, "720p/redirect.m3u8", "")
	if redirect.Code != http.StatusBadGateway || redirect.Header().Get("Location") != "" {
		t.Fatalf("upstream redirect=%d headers=%v", redirect.Code, redirect.Header())
	}
}

func TestHLSHandlerRejectsUndeclaredTraversalQueryAndTerminalSession(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { upstreamCalls++ }))
	defer upstream.Close()
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x7b}, 32))
	record, _ := createRuntimeSessionV1(t, store, "sess_hls_002", "runner_hls_002")
	handler, _ := NewHLSHandlerV1(store, testLivePresetsV1(), upstream.URL, nil, time.Second)
	for _, test := range []struct{ asset, query string }{
		{"1080p/index.m3u8", ""},
		{"720p/../index.m3u8", ""},
		{"720p/index.m3u8", "token=unsafe"},
		{"720p/index.m3u8", "_HLS_msn=not-a-number"},
		{"720p/secret.json", ""},
	} {
		response := hlsRequestV1(t, handler, record.RunnerSessionID, test.asset, test.query)
		if response.Code != http.StatusNotFound {
			t.Fatalf("asset=%q query=%q status=%d", test.asset, test.query, response.Code)
		}
	}
	stopping, _, err := store.BeginTermination(record.BrokerSessionID, "gateway_close")
	if err != nil || !stopping.Stopping {
		t.Fatal(err)
	}
	if response := hlsRequestV1(t, handler, record.RunnerSessionID, "master.m3u8", ""); response.Code != http.StatusNotFound {
		t.Fatalf("stopping master status=%d", response.Code)
	}
	if upstreamCalls != 0 {
		t.Fatalf("rejected HLS requests reached upstream %d times", upstreamCalls)
	}
}

func TestHLSMasterListsOnlyPlayableRenditionsAndReturnsUnavailable(t *testing.T) {
	var playable atomic.Bool
	playable.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !playable.Load() || strings.Contains(request.URL.Path, "/360p/") {
			http.NotFound(writer, request)
			return
		}
		if strings.HasSuffix(request.URL.Path, "/index.m3u8") {
			_, _ = io.WriteString(writer, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nvideo.m3u8\n")
			return
		}
		if strings.HasSuffix(request.URL.Path, "/video.m3u8") {
			_, _ = io.WriteString(writer, "#EXTM3U\n#EXTINF:1.0,\nsegment.mp4\n")
			return
		}
		http.NotFound(writer, request)
	}))
	defer upstream.Close()
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x7c}, 32))
	record, _ := createRuntimeSessionV1(t, store, "sess_hls_health", "runner_hls_health")
	handler, err := NewHLSHandlerV1(store, testLivePresetsV1(), upstream.URL, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	master := hlsRequestV1(t, handler, record.RunnerSessionID, "master.m3u8", "")
	if master.Code != http.StatusOK || !strings.Contains(master.Body.String(), "720p/index.m3u8") || strings.Contains(master.Body.String(), "360p/index.m3u8") {
		t.Fatalf("filtered master=%d %q", master.Code, master.Body.String())
	}
	playable.Store(false)
	unavailable := hlsRequestV1(t, handler, record.RunnerSessionID, "master.m3u8", "")
	if unavailable.Code != http.StatusServiceUnavailable || unavailable.Header().Get("Retry-After") != "1" || !strings.Contains(unavailable.Body.String(), "output_unavailable") {
		t.Fatalf("unavailable master=%d headers=%v body=%q", unavailable.Code, unavailable.Header(), unavailable.Body.String())
	}
}

func hlsRequestV1(t *testing.T, handler http.Handler, runnerID, asset, query string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/v1/public/sessions/" + runnerID + "/" + asset
	if query != "" {
		target += "?" + query
	}
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.SetPathValue("id", runnerID)
	request.SetPathValue("asset", asset)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
