package liverunner

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPublisherDeadlinesSurviveRestartAndReconnect(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{0x59}, 32)
	store := newTestStoreV1(t, dir, key)
	record, _ := createRuntimeSessionV1(t, store, "sess_idle", "runner_idle")
	start, _ := time.Parse(time.RFC3339Nano, record.OutputStateSince)
	if expired, err := store.ExpireAbsentPublisher(record.BrokerSessionID, start, 5*time.Minute, 2*time.Minute); err != nil || expired {
		t.Fatalf("initial: %v %v", expired, err)
	}
	store = newTestStoreV1(t, dir, key)
	// Changed config must not reset an already issued deadline.
	if expired, err := store.ExpireAbsentPublisher(record.BrokerSessionID, start.Add(5*time.Minute), time.Hour, time.Hour); err != nil || !expired {
		t.Fatalf("restart: %v %v", expired, err)
	}
	ended, err := FinalizeLiveTerminationV1(store, record.BrokerSessionID, start.Add(5*time.Minute))
	if err != nil || ended.State != "ended" || ended.CloseReason != "publisher_disconnect" || ended.UsageTotal != 0 {
		t.Fatalf("end: %+v %v", ended, err)
	}
	if _, err = FinalizeLiveTerminationV1(store, record.BrokerSessionID, start.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}

	record, _ = createRuntimeSessionV1(t, store, "sess_reconnect", "runner_reconnect")
	start = time.Now().UTC()
	if err := store.RecordIngestPresence(record.BrokerSessionID, true, start); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordIngestPresence(record.BrokerSessionID, false, start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if expired, err := store.ExpireAbsentPublisher(record.BrokerSessionID, start.Add(time.Minute), 5*time.Minute, 2*time.Minute); err != nil || expired {
		t.Fatalf("grace: %v %v", expired, err)
	}
	store = newTestStoreV1(t, dir, key)
	if err := store.RecordIngestPresence(record.BrokerSessionID, true, start.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	if expired, err := store.ExpireAbsentPublisher(record.BrokerSessionID, start.Add(3*time.Minute), 5*time.Minute, 2*time.Minute); err != nil || expired {
		t.Fatalf("stale deadline: %v %v", expired, err)
	}
	if err := store.RecordIngestPresence(record.BrokerSessionID, false, start.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if expired, err := store.ExpireAbsentPublisher(record.BrokerSessionID, start.Add(6*time.Minute), 5*time.Minute, 2*time.Minute); err != nil || !expired {
		t.Fatalf("second disconnect: %v %v", expired, err)
	}
}

type unavailablePublisherV1 struct{}

func (unavailablePublisherV1) Path(context.Context, string) (MediaPathStatusV1, error) {
	return MediaPathStatusV1{}, errors.New("router unavailable")
}

func TestUnknownRouterStateDoesNotExpirePublisher(t *testing.T) {
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x59}, 32))
	record, _ := createRuntimeSessionV1(t, store, "sess_unknown", "runner_unknown")
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	hls, err := NewHLSHandlerV1(store, testLivePresetsV1(), upstream.URL, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	meter, err := NewLiveOutputMeterV1(store, hls, unavailablePublisherV1{}, time.Millisecond, time.Second, time.Second, time.Second, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	meter.now = func() time.Time { return time.Now().Add(time.Hour) }
	if meter.poll(context.Background(), record, "ingest/runner_unknown", "renditions/runner_unknown/720p") {
		t.Fatal("router outage ended session")
	}
	saved, _, _ := store.Load(record.BrokerSessionID)
	if saved.Stopping || saved.PublisherDeadline != "" {
		t.Fatal("unknown observation changed deadline")
	}
}

func TestNeverPublishedSessionEndsAndReleasesCapacity(t *testing.T) {
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x59}, 32))
	record, secrets := createRuntimeSessionV1(t, store, "sess_never", "runner_never")

	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	hls, err := NewHLSHandlerV1(store, testLivePresetsV1(), upstream.URL, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	router := fakePublisherWaiterV1{ready: make(chan struct{})}
	meter, err := NewLiveOutputMeterV1(store, hls, router, time.Millisecond, time.Second, time.Second, time.Second, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	meter.now = func() time.Time { return time.Now().Add(6 * time.Minute) }
	coordinator := newTestRuntimeCoordinatorV1(t, store, router, &fakeLiveLauncherV1{}, 1)
	coordinator.meter = meter
	defer coordinator.Shutdown(context.Background())
	if err := coordinator.EnsureSession(context.Background(), record, secrets); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		saved, _, err := store.Load(record.BrokerSessionID)
		if err != nil {
			t.Fatal(err)
		}
		if saved.State == "ended" && len(coordinator.capacity) == 0 {
			if saved.CloseReason != "publisher_disconnect" || saved.UsageTotal != 0 || saved.PendingEvents[len(saved.PendingEvents)-1].EventType != "session.ended" {
				t.Fatalf("terminal: %+v", saved)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("never-published runtime did not terminate/release capacity")
}
