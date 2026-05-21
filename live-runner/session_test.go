package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type testCloser struct {
	closed bool
}

func (c *testCloser) Close() error {
	c.closed = true
	return nil
}

func TestMarkProgressTransitionsToPublishing(t *testing.T) {
	rt := &sessionRuntime{rec: sessionRecord{State: stateReady}}
	delta, started := rt.markProgress(5)
	if !started || delta != 5 {
		t.Fatalf("started=%v delta=%d", started, delta)
	}
	if rt.rec.State != statePublishing {
		t.Fatalf("state=%s", rt.rec.State)
	}
}

func TestWatchdogNoPublishTimeout(t *testing.T) {
	rt := &sessionRuntime{
		rec: sessionRecord{
			RunnerSessionID: "a",
			State:           stateReady,
			CreatedAt:       time.Now().Add(-2 * time.Minute),
		},
	}
	if err := rt.watchdog(10 * time.Second); err == nil {
		t.Fatal("expected timeout")
	}
}

func TestLogTailKeepsRecentLines(t *testing.T) {
	tail := newLogTail(3)
	tail.add("a")
	tail.add("b")
	tail.add("c")
	tail.add("d")
	if got := tail.join(); got != "b | c | d" {
		t.Fatalf("tail=%q", got)
	}
}

func TestScanCRLFSplitsProgressLines(t *testing.T) {
	scanner := bufio.NewScanner(bytes.NewBufferString("frame=   70 fps= 47\rframe=   85 fps= 42\nfinal"))
	scanner.Split(scanCRLF)
	var got []string
	for scanner.Scan() {
		got = append(got, scanner.Text())
	}
	if len(got) != 3 || got[0] != "frame=   70 fps= 47" || got[1] != "frame=   85 fps= 42" || got[2] != "final" {
		t.Fatalf("tokens=%q", got)
	}
}

func TestRedactSecrets(t *testing.T) {
	got := redactSecrets("rtmp://host/live/secret1234 token", "secret1234")
	if got != "rtmp://host/live/[redacted:1234] token" {
		t.Fatalf("redacted=%q", got)
	}
}

func TestStaleRemoteFilesDeletesOnlySegments(t *testing.T) {
	remote := map[string]struct{}{
		"master.m3u8":      {},
		"v0/playlist.m3u8": {},
		"v0/seg0.ts":       {},
		"v0/seg1.ts":       {},
		"v0/init.mp4":      {},
	}
	present := map[string]struct{}{
		"master.m3u8":      {},
		"v0/playlist.m3u8": {},
		"v0/seg1.ts":       {},
		"v0/init.mp4":      {},
	}
	stale, err := staleRemoteFiles(remote, present)
	if err != nil {
		t.Fatalf("staleRemoteFiles: %v", err)
	}
	if len(stale) != 1 || stale[0] != "v0/seg0.ts" {
		t.Fatalf("stale=%v", stale)
	}
}

func TestSharedIngestPortDefault(t *testing.T) {
	cfg := config{SharedIngestAddr: ":1935"}
	if got := cfg.sharedIngestPort(); got != "1935" {
		t.Fatalf("port=%q", got)
	}
}

func TestEmitPublishStoppedIsDeduplicatedUntilPublisherReturns(t *testing.T) {
	events, closeFn := newEventCollector(t)
	defer closeFn()

	rt := &sessionRuntime{
		rec: sessionRecord{
			RunnerSessionID: "rsess_test",
			BrokerSessionID: "bsess_test",
			Callbacks: brokerCallbackConfig{
				EventURL: events.server.URL,
			},
		},
		callbacks: &callbackClient{httpClient: events.client},
	}

	rt.emitPublishStopped("publisher_disconnected", nil)
	rt.emitPublishStopped("publisher_disconnected", nil)
	waitForEventCount(t, events, "session.publish_stopped", 1)

	rt.notePublisherAccepted()
	rt.emitPublishStopped("publisher_disconnected", nil)
	waitForEventCount(t, events, "session.publish_stopped", 2)
}

func TestEmitUploadFailedIsDeduplicatedUntilRecovery(t *testing.T) {
	events, closeFn := newEventCollector(t)
	defer closeFn()

	rt := &sessionRuntime{
		rec: sessionRecord{
			RunnerSessionID: "rsess_test",
			BrokerSessionID: "bsess_test",
			Callbacks: brokerCallbackConfig{
				EventURL: events.server.URL,
			},
		},
		callbacks: &callbackClient{httpClient: events.client},
	}

	rt.emitUploadFailed(map[string]any{"error_text": "boom"})
	rt.emitUploadFailed(map[string]any{"error_text": "boom"})
	waitForEventCount(t, events, "session.upload.failed", 1)

	rt.clearUploadFailure()
	rt.emitUploadFailed(map[string]any{"error_text": "boom"})
	waitForEventCount(t, events, "session.upload.failed", 2)
}

func TestShouldLogFFmpegLineSuppressesBoilerplate(t *testing.T) {
	cases := map[string]bool{
		"ffmpeg version n7.1.3":                                  false,
		"configuration: --prefix=/usr/local":                    false,
		"libavutil      59. 39.100 / 59. 39.100":                false,
		"[hls @ 0x1] Opening '/tmp/live/rsess/v0/segment.ts'":   false,
		"[swscaler @ 0x1] deprecated pixel format used":         false,
		"[h264 @ 0x1] mmco: unref short failure":                true,
		"Error opening input files: Connection refused":         true,
	}
	for line, want := range cases {
		if got := shouldLogFFmpegLine(line); got != want {
			t.Fatalf("line=%q got=%v want=%v", line, got, want)
		}
	}
}

func TestPublisherDisconnectDoesNotCloseIngestPipe(t *testing.T) {
	pipe := &testCloser{}
	rt := &sessionRuntime{
		rec: sessionRecord{
			RunnerSessionID: "rsess_test",
			State:           statePublishing,
			Ingest: liveIngestStatus{
				ConnectedPublisher: true,
			},
		},
		ingestPipe: pipe,
	}

	rt.notePublisherDisconnected("publisher_disconnected")

	if pipe.closed {
		t.Fatal("expected disconnect to preserve ingest pipe for reconnect window")
	}
	if rt.rec.State != stateStalled {
		t.Fatalf("state=%s", rt.rec.State)
	}
	if rt.rec.Ingest.ConnectedPublisher {
		t.Fatal("expected publisher to be marked disconnected")
	}
}

func TestFormatAuthorizationScopeRedactsAccessKey(t *testing.T) {
	auth := "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE1234/20260520/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=abcd"
	got := formatAuthorizationScope(auth)
	if got != "1234/20260520/us-east-1/s3/aws4_request" {
		t.Fatalf("scope=%q", got)
	}
}

type collectedEvents struct {
	client *http.Client
	server *httptest.Server
	mu     sync.Mutex
	types  []string
}

func newEventCollector(t *testing.T) (*collectedEvents, func()) {
	t.Helper()
	c := &collectedEvents{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var env eventEnvelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Errorf("decode event: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.types = append(c.types, env.EventType)
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	c.client = c.server.Client()
	return c, func() { c.server.Close() }
}

func waitForEventCount(t *testing.T, c *collectedEvents, eventType string, want int) {
	t.Helper()
	waitForCondition(t, 2*time.Second, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		count := 0
		for _, got := range c.types {
			if got == eventType {
				count++
			}
		}
		return count == want
	})
}
