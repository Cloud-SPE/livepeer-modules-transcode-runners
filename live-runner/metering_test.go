package liverunner

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseFinalizedHLSSegmentsIgnoresPartsAndAggregatesExactDurations(t *testing.T) {
	playlist := "\ufeff#EXTM3U\r\n" +
		"#EXT-X-VERSION:9\r\n" +
		"#EXT-X-MEDIA-SEQUENCE:41\r\n" +
		"#EXT-X-PART:DURATION=0.20000,URI=part0.mp4\r\n" +
		"#EXTINF:0.333333,\r\nsegment41.mp4\r\n" +
		"#EXT-X-PART:DURATION=0.2,URI=part1.mp4\r\n" +
		"#EXTINF:0.666667,title\r\nsegment42.mp4\r\n"
	segments, err := ParseFinalizedHLSSegmentsV1(strings.NewReader(playlist))
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 || segments[0].URI != "segment41.mp4" || segments[0].DurationMicroseconds != 333333 || segments[1].URI != "segment42.mp4" || segments[1].DurationMicroseconds != 666667 {
		t.Fatalf("segments=%+v", segments)
	}
	durations := []uint64{segments[0].DurationMicroseconds, segments[1].DurationMicroseconds}
	if total, err := CalculateOutputSecondsV1(durations); err != nil || total != 1 {
		t.Fatalf("total=%d err=%v", total, err)
	}
}

func TestParseFinalizedHLSSegmentsDeduplicatesPlaylistEntries(t *testing.T) {
	playlist := "#EXTM3U\n#EXTINF:1.0,\nsegment.mp4\n#EXTINF:1.0,\nsegment.mp4\n"
	segments, err := ParseFinalizedHLSSegmentsV1(strings.NewReader(playlist))
	if err != nil || len(segments) != 1 {
		t.Fatalf("segments=%+v err=%v", segments, err)
	}
}

func TestParseFinalizedHLSSegmentsRejectsMalformedOrAmbiguousInput(t *testing.T) {
	tests := []string{
		"not-hls\n",
		"#EXTM3U\n#EXT-X-PART:DURATION=1,URI=part.mp4\n",
		"#EXTM3U\n#EXTINF:1.0000001,\nsegment.mp4\n",
		"#EXTM3U\n#EXTINF:1,\n",
		"#EXTM3U\n#EXTINF:1,\nsegment.mp4\n#EXTINF:2,\nsegment.mp4\n",
	}
	for _, playlist := range tests {
		if segments, err := ParseFinalizedHLSSegmentsV1(strings.NewReader(playlist)); err == nil {
			t.Fatalf("accepted malformed playlist %q as %+v", playlist, segments)
		}
	}
}

func TestLiveOutputMeterReadsDeclaredMediaPlaylistAndAdvancesOnce(t *testing.T) {
	var server *httptest.Server
	mediaRequests := 0
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Cookie") != "hls_auth=ok" {
			http.SetCookie(writer, &http.Cookie{Name: "hls_auth", Value: "ok"})
			http.Redirect(writer, request, server.URL+request.URL.Path+"?cookieCheck=1", http.StatusFound)
			return
		}
		switch request.URL.Path {
		case "/renditions/runner_meter_001/720p/index.m3u8":
			_, _ = io.WriteString(writer, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nvideo1_stream.m3u8\n")
		case "/renditions/runner_meter_001/720p/video1_stream.m3u8":
			mediaRequests++
			if mediaRequests == 1 {
				_, _ = io.WriteString(writer, "#EXTM3U\n#EXT-X-PART:DURATION=0.2,URI=part.mp4\n")
				return
			}
			_, _ = io.WriteString(writer, "#EXTM3U\n#EXT-X-PART:DURATION=0.2,URI=part.mp4\n#EXTINF:1.0,\nsegment.mp4\n")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x5f}, 32))
	record, _ := createRuntimeSessionV1(t, store, "sess_meter_001", "runner_meter_001")
	if err := store.Advance(record.BrokerSessionID, testEventV1(record.RunnerSessionID, 1, "session.started", "active", 0, "")); err != nil {
		t.Fatal(err)
	}
	hls, err := NewHLSHandlerV1(store, testLivePresetsV1(), server.URL, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	close(ready)
	meter, err := NewLiveOutputMeterV1(store, hls, fakePublisherWaiterV1{ready: ready}, time.Millisecond, time.Hour, time.Second, 20*time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	meter.now = func() time.Time { return time.Date(2026, 8, 24, 13, 0, 0, 0, time.UTC) }
	meter.poll(context.Background(), record, "ingest/runner_meter_001", "renditions/runner_meter_001/720p")
	meter.poll(context.Background(), record, "ingest/runner_meter_001", "renditions/runner_meter_001/720p")
	persisted, _, err := store.Load(record.BrokerSessionID)
	if err != nil || persisted.UsageTotal != 1 || persisted.LastSequence != 3 || len(persisted.PendingEvents) != 3 || persisted.PendingEvents[1].EventType != "session.heartbeat" || len(persisted.MeteredSegmentSHA256) != 1 {
		t.Fatalf("metered record=%+v err=%v", persisted, err)
	}
}

func TestLiveOutputMeterCountsStalledTransitionOnce(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x6f}, 32))
	record, _ := createRuntimeSessionV1(t, store, "sess_meter_stalled", "runner_meter_stalled")
	startedAt := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	started := testEventV1(record.RunnerSessionID, 1, "session.started", "active", 0, "")
	started.EventTime = startedAt.Format(time.RFC3339Nano)
	if err := store.Advance(record.BrokerSessionID, started); err != nil {
		t.Fatal(err)
	}
	hls, err := NewHLSHandlerV1(store, testLivePresetsV1(), upstream.URL, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	close(ready)
	meter, err := NewLiveOutputMeterV1(store, hls, fakePublisherWaiterV1{ready: ready}, time.Millisecond, time.Hour, time.Second, 20*time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := startedAt
	meter.now = func() time.Time { return now }
	meter.poll(context.Background(), record, "ingest/runner_meter_stalled", "renditions/runner_meter_stalled/720p")
	now = startedAt.Add(20 * time.Second)
	meter.poll(context.Background(), record, "ingest/runner_meter_stalled", "renditions/runner_meter_stalled/720p")
	meter.poll(context.Background(), record, "ingest/runner_meter_stalled", "renditions/runner_meter_stalled/720p")
	if meter.metrics.sessionsStalled.Load() != 1 {
		t.Fatalf("stalled metric=%d", meter.metrics.sessionsStalled.Load())
	}
}
