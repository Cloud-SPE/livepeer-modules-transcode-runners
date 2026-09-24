package liverunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCallbackDispatcherRetriesWithoutChangingEventIdentity(t *testing.T) {
	var attempts atomic.Int32
	var firstBody []byte
	broker := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(request.Body)
		if request.Header.Get("Authorization") != "Bearer callback-secret-fixture" || request.Header.Get("Content-Type") != "application/json" {
			t.Error("callback credentials or media type missing")
		}
		if attempts.Add(1) == 1 {
			firstBody = append([]byte(nil), body.Bytes()...)
			http.Error(writer, "temporary", http.StatusServiceUnavailable)
			return
		}
		if !bytes.Equal(firstBody, body.Bytes()) {
			t.Error("callback retry changed the event envelope")
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"accepted": true, "duplicate": true})
	}))
	defer broker.Close()

	store, request, response := callbackTestSessionV1(t, broker.URL)
	event := testEventV1(response.RunnerSessionID, 1, "session.usage.tick", "active", 7, "")
	if err := store.Advance(request.SessionID, event); err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewCallbackDispatcherV1(store, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	dispatcher.now = func() time.Time { return now }
	delivered, err := dispatcher.DeliverNext(context.Background(), request.SessionID)
	var deliveryError *CallbackDeliveryErrorV1
	if delivered || !errors.As(err, &deliveryError) || !deliveryError.Retryable || deliveryError.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("first delivery=%v err=%+v", delivered, err)
	}
	record, _, err := store.Load(request.SessionID)
	if err != nil || len(record.PendingEvents) != 1 || record.PendingEvents[0].EventID != event.EventID {
		t.Fatalf("retry outbox=%+v err=%v", record.PendingEvents, err)
	}
	now, err = time.Parse(time.RFC3339Nano, record.CallbackDelivery.NextAttemptAt)
	if err != nil {
		t.Fatal(err)
	}
	delivered, err = dispatcher.DeliverNext(context.Background(), request.SessionID)
	if err != nil || !delivered {
		t.Fatalf("retry delivery=%v err=%v", delivered, err)
	}
	record, _, err = store.Load(request.SessionID)
	if err != nil || len(record.PendingEvents) != 0 || attempts.Load() != 2 {
		t.Fatalf("final outbox=%+v attempts=%d err=%v", record.PendingEvents, attempts.Load(), err)
	}
}

func TestCallbackDispatcherPreservesOutboxOrder(t *testing.T) {
	var sequences []uint64
	broker := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var event RunnerEventV1
		if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
			t.Error(err)
		}
		sequences = append(sequences, event.Sequence)
		writer.WriteHeader(http.StatusOK)
	}))
	defer broker.Close()
	store, request, response := callbackTestSessionV1(t, broker.URL)
	for sequence := uint64(1); sequence <= 2; sequence++ {
		event := testEventV1(response.RunnerSessionID, sequence, "session.heartbeat", "active", 0, "")
		if err := store.Advance(request.SessionID, event); err != nil {
			t.Fatal(err)
		}
	}
	dispatcher, _ := NewCallbackDispatcherV1(store, nil, time.Second)
	for range 2 {
		if delivered, err := dispatcher.DeliverNext(context.Background(), request.SessionID); err != nil || !delivered {
			t.Fatalf("delivery=%v err=%v", delivered, err)
		}
	}
	if len(sequences) != 2 || sequences[0] != 1 || sequences[1] != 2 {
		t.Fatalf("callback sequence=%v", sequences)
	}
}

func TestCallbackDispatcherRejectsRedirectsAndClassifiesPermanentFailures(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Store(true) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	store, request, response := callbackTestSessionV1(t, redirect.URL)
	if err := store.Advance(request.SessionID, testEventV1(response.RunnerSessionID, 1, "session.heartbeat", "active", 0, "")); err != nil {
		t.Fatal(err)
	}
	dispatcher, _ := NewCallbackDispatcherV1(store, nil, time.Second)
	_, err := dispatcher.DeliverNext(context.Background(), request.SessionID)
	var deliveryError *CallbackDeliveryErrorV1
	if !errors.As(err, &deliveryError) || deliveryError.Retryable || deliveryError.StatusCode != http.StatusTemporaryRedirect || redirected.Load() {
		t.Fatalf("redirect error=%+v followed=%v", err, redirected.Load())
	}
	record, _, loadErr := store.Load(request.SessionID)
	if loadErr != nil || len(record.PendingEvents) != 0 || len(record.CallbackDeadLetters) != 1 || record.CallbackDeadLetters[0].ErrorCode != "http_307" {
		t.Fatalf("redirect audit=%+v loadErr=%v", record.CallbackDeadLetters, loadErr)
	}
}

func TestCallbackDispatcherDoesNotEchoCallbackCoordinates(t *testing.T) {
	store, request, response := callbackTestSessionV1(t, "http://127.0.0.1:1/path-containing-secret")
	if err := store.Advance(request.SessionID, testEventV1(response.RunnerSessionID, 1, "session.heartbeat", "active", 0, "")); err != nil {
		t.Fatal(err)
	}
	dispatcher, _ := NewCallbackDispatcherV1(store, nil, 50*time.Millisecond)
	_, err := dispatcher.DeliverNext(context.Background(), request.SessionID)
	if err == nil || strings.Contains(err.Error(), "path-containing-secret") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("unsafe callback error=%v", err)
	}
}

func TestCallbackWorkerRetriesTerminalOutboxThenErasesSecrets(t *testing.T) {
	var attempts atomic.Int32
	var sequences []uint64
	broker := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var event RunnerEventV1
		if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
			t.Error(err)
		}
		attempt := attempts.Add(1)
		if attempt == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		sequences = append(sequences, event.Sequence)
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer broker.Close()
	store, request, response := callbackTestSessionV1(t, broker.URL)
	if err := store.Advance(request.SessionID, testEventV1(response.RunnerSessionID, 1, "session.started", "active", 0, "")); err != nil {
		t.Fatal(err)
	}
	record, _, err := store.BeginTermination(request.SessionID, "gateway_close")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Advance(request.SessionID, testEventV1(response.RunnerSessionID, record.LastSequence+1, "session.ended", "ended", 0, "gateway_close")); err != nil {
		t.Fatal(err)
	}
	dispatcher, _ := NewCallbackDispatcherV1(store, nil, time.Second)
	worker, _ := NewCallbackWorkerV1(store, dispatcher, 5*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	for {
		persisted, secrets, loadErr := store.Load(request.SessionID)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if len(persisted.PendingEvents) == 0 && secrets == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("worker exited before drain: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 3 || len(sequences) != 2 || sequences[0] != 1 || sequences[1] != 2 {
		t.Fatalf("attempts=%d acknowledged sequences=%v", attempts.Load(), sequences)
	}
}

func TestCallbackWorkerRecoversAcknowledgedTerminalSecretErase(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) }))
	defer broker.Close()
	store, request, response := callbackTestSessionV1(t, broker.URL)
	record, _, err := store.BeginTermination(request.SessionID, "gateway_close")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Advance(request.SessionID, testEventV1(response.RunnerSessionID, record.LastSequence+1, "session.ended", "ended", 0, "gateway_close")); err != nil {
		t.Fatal(err)
	}
	dispatcher, _ := NewCallbackDispatcherV1(store, nil, time.Second)
	if delivered, err := dispatcher.DeliverNext(context.Background(), request.SessionID); err != nil || !delivered {
		t.Fatalf("delivery=%v err=%v", delivered, err)
	}
	before, secrets, err := store.Load(request.SessionID)
	if err != nil || secrets == nil || len(before.PendingEvents) != 0 {
		t.Fatalf("pre-recovery record=%+v secrets=%+v err=%v", before, secrets, err)
	}
	restarted := newTestStoreV1(t, store.dir, bytes.Repeat([]byte{0x55}, 32))
	restartedDispatcher, _ := NewCallbackDispatcherV1(restarted, nil, time.Second)
	worker, _ := NewCallbackWorkerV1(restarted, restartedDispatcher, time.Second)
	if err := worker.SweepOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, secrets, err := restarted.Load(request.SessionID)
	if err != nil || secrets != nil || after.WrappedKeyCiphertext != "" {
		t.Fatalf("recovered record=%+v secrets=%+v err=%v", after, secrets, err)
	}
}

func TestCallbackWorkerParksPermanentFailureOnce(t *testing.T) {
	var attempts atomic.Int32
	broker := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer broker.Close()
	store, request, response := callbackTestSessionV1(t, broker.URL)
	if err := store.Advance(request.SessionID, testEventV1(response.RunnerSessionID, 1, "session.heartbeat", "active", 0, "")); err != nil {
		t.Fatal(err)
	}
	dispatcher, _ := NewCallbackDispatcherV1(store, nil, time.Second)
	var counted atomic.Int32
	dispatcher.rejectionCount = func(statusCode int, code string) {
		if statusCode != http.StatusUnauthorized || code != "callback_auth_rejected" {
			t.Errorf("unexpected rejection metric %d/%s", statusCode, code)
		}
		counted.Add(1)
	}
	worker, _ := NewCallbackWorkerV1(store, dispatcher, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for attempts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	record, _, err := store.Load(request.SessionID)
	if err != nil || attempts.Load() != 1 || counted.Load() != 1 || len(record.PendingEvents) != 0 || record.CallbackRejectedTotal != 1 || len(record.CallbackDeadLetters) != 1 || record.CallbackDeadLetters[0].Event.EventID != response.RunnerSessionID+":1" {
		t.Fatalf("attempts=%d record=%+v err=%v", attempts.Load(), record, err)
	}
}

func TestCallbackWorkerParks409AndDeliversLaterUsageAndTerminalEvents(t *testing.T) {
	var received []uint64
	broker := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var event RunnerEventV1
		if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
			t.Error(err)
			return
		}
		if event.Sequence == 1 {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusConflict)
			_, _ = writer.Write([]byte(`{"error":"event_not_supported"}`))
			return
		}
		received = append(received, event.Sequence)
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer broker.Close()

	store, request, response := callbackTestSessionV1(t, broker.URL)
	if err := store.Advance(request.SessionID, testEventV1(response.RunnerSessionID, 1, "session.started", "active", 0, "")); err != nil {
		t.Fatal(err)
	}
	if err := store.Advance(request.SessionID, testEventV1(response.RunnerSessionID, 2, "session.usage.tick", "active", 7, "")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.BeginTermination(request.SessionID, "gateway_close"); err != nil {
		t.Fatal(err)
	}
	if err := store.Advance(request.SessionID, testEventV1(response.RunnerSessionID, 3, "session.ended", "ended", 7, "gateway_close")); err != nil {
		t.Fatal(err)
	}
	dispatcher, _ := NewCallbackDispatcherV1(store, nil, time.Second)
	worker, _ := NewCallbackWorkerV1(store, dispatcher, time.Second)
	if err := worker.SweepOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	record, secrets, err := store.Load(request.SessionID)
	if err != nil || secrets != nil || len(record.PendingEvents) != 0 || record.CallbackRejectedTotal != 1 || len(record.CallbackDeadLetters) != 1 {
		t.Fatalf("drained record=%+v secrets=%+v err=%v", record, secrets, err)
	}
	if record.CallbackDeadLetters[0].ErrorCode != "event_not_supported" || len(received) != 2 || received[0] != 2 || received[1] != 3 {
		t.Fatalf("dead letters=%+v delivered=%v", record.CallbackDeadLetters, received)
	}
}

func TestCallbackRetryBackoffSurvivesRestart(t *testing.T) {
	var attempts atomic.Int32
	var firstBody []byte
	broker := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if attempts.Add(1) == 1 {
			firstBody = append([]byte(nil), body...)
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if !bytes.Equal(firstBody, body) {
			t.Error("restarted retry changed event identity")
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer broker.Close()

	store, request, response := callbackTestSessionV1(t, broker.URL)
	if err := store.Advance(request.SessionID, testEventV1(response.RunnerSessionID, 1, "session.heartbeat", "active", 0, "")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	dispatcher, _ := NewCallbackDispatcherV1(store, nil, time.Second)
	dispatcher.now = func() time.Time { return now }
	if delivered, err := dispatcher.DeliverNext(context.Background(), request.SessionID); delivered || err == nil {
		t.Fatalf("initial delivery=%v err=%v", delivered, err)
	}
	restarted := newTestStoreV1(t, store.dir, bytes.Repeat([]byte{0x55}, 32))
	record, _, err := restarted.Load(request.SessionID)
	if err != nil || record.CallbackDelivery == nil || record.CallbackDelivery.Attempts != 1 || record.CallbackDelivery.LastStatusCode != http.StatusServiceUnavailable {
		t.Fatalf("persisted callback attempt=%+v err=%v", record.CallbackDelivery, err)
	}
	next, _ := time.Parse(time.RFC3339Nano, record.CallbackDelivery.NextAttemptAt)
	restartedDispatcher, _ := NewCallbackDispatcherV1(restarted, nil, time.Second)
	restartedDispatcher.now = func() time.Time { return now }
	if delivered, err := restartedDispatcher.DeliverNext(context.Background(), request.SessionID); err != nil || delivered || attempts.Load() != 1 {
		t.Fatalf("early retry delivery=%v attempts=%d err=%v", delivered, attempts.Load(), err)
	}
	now = next
	if delivered, err := restartedDispatcher.DeliverNext(context.Background(), request.SessionID); err != nil || !delivered || attempts.Load() != 2 {
		t.Fatalf("due retry delivery=%v attempts=%d err=%v", delivered, attempts.Load(), err)
	}
}

func TestCallbackPermanentResponsePolicies(t *testing.T) {
	tests := []struct {
		status int
		code   string
	}{
		{http.StatusUnauthorized, "callback_auth_rejected"},
		{http.StatusForbidden, "callback_auth_rejected"},
		{http.StatusNotFound, "broker_session_terminal"},
		{http.StatusGone, "broker_session_terminal"},
	}
	for _, test := range tests {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			broker := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(test.status) }))
			defer broker.Close()
			store, request, response := callbackTestSessionV1(t, broker.URL)
			if err := store.Advance(request.SessionID, testEventV1(response.RunnerSessionID, 1, "session.heartbeat", "active", 0, "")); err != nil {
				t.Fatal(err)
			}
			dispatcher, _ := NewCallbackDispatcherV1(store, nil, time.Second)
			_, err := dispatcher.DeliverNext(context.Background(), request.SessionID)
			var deliveryError *CallbackDeliveryErrorV1
			if !errors.As(err, &deliveryError) || deliveryError.Retryable || deliveryError.Code != test.code {
				t.Fatalf("delivery error=%+v", err)
			}
		})
	}
}

func TestCallbackErrorCodeDoesNotTrustArbitraryBrokerText(t *testing.T) {
	if got := safeCallbackErrorCodeV1(http.StatusConflict, []byte(`{"error":"customer_secret_value"}`)); got != "http_409" {
		t.Fatalf("untrusted broker code=%q", got)
	}
	if got := safeCallbackErrorCodeV1(http.StatusConflict, []byte(`{"error":"sequence_conflict"}`)); got != "sequence_conflict" {
		t.Fatalf("known broker code=%q", got)
	}
}

func TestCallbackDeadLetterDoesNotPersistBrokerResponseText(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusConflict)
		_, _ = writer.Write([]byte(`{"error":"customer_secret_value"}`))
	}))
	defer broker.Close()
	store, request, response := callbackTestSessionV1(t, broker.URL)
	if err := store.Advance(request.SessionID, testEventV1(response.RunnerSessionID, 1, "session.heartbeat", "active", 0, "")); err != nil {
		t.Fatal(err)
	}
	dispatcher, _ := NewCallbackDispatcherV1(store, nil, time.Second)
	if _, err := dispatcher.DeliverNext(context.Background(), request.SessionID); err == nil {
		t.Fatal("permanent callback rejection returned no error")
	}
	body, err := os.ReadFile(store.path(request.SessionID))
	if err != nil || bytes.Contains(body, []byte("customer_secret_value")) {
		t.Fatalf("unsafe callback response persisted: err=%v", err)
	}
	record, _, err := store.Load(request.SessionID)
	if err != nil || len(record.CallbackDeadLetters) != 1 || record.CallbackDeadLetters[0].ErrorCode != "http_409" {
		t.Fatalf("safe callback audit=%+v err=%v", record.CallbackDeadLetters, err)
	}
}

func TestCallbackRetryPolicyHasBoundedStableJitter(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		if !callbackStatusRetryableV1(status) {
			t.Fatalf("status %d was not retryable", status)
		}
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusConflict, http.StatusGone, http.StatusTemporaryRedirect} {
		if callbackStatusRetryableV1(status) {
			t.Fatalf("status %d was retryable", status)
		}
	}
	initial, maximum := 500*time.Millisecond, 30*time.Second
	previous := time.Duration(0)
	for attempt := uint32(1); attempt <= 20; attempt++ {
		delay := callbackRetryDelayV1("runner_event:1", attempt, initial, maximum)
		if delay <= 0 || delay > maximum || delay != callbackRetryDelayV1("runner_event:1", attempt, initial, maximum) {
			t.Fatalf("attempt %d delay=%s", attempt, delay)
		}
		if attempt > 1 && delay < previous/2 {
			t.Fatalf("attempt %d unexpectedly collapsed from %s to %s", attempt, previous, delay)
		}
		previous = delay
	}
}

func TestLiveRunnerMetricsCountsCallbackRejectionsByBoundedStatus(t *testing.T) {
	metrics := &LiveRunnerMetricsV1{}
	metrics.RecordCallbackRejected(http.StatusConflict, "body_is_not_a_label")
	metrics.RecordCallbackRejected(http.StatusConflict, "different_body")
	metrics.RecordLadderStart("started")
	metrics.RecordLadderStart("encoder_init_failed")
	metrics.RecordLadderExit("process_killed")
	metrics.RecordSessionStalled()
	metrics.RecordGPUProbe("available")
	recorder := httptest.NewRecorder()
	metrics.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	for _, want := range []string{`live_callback_rejected_total{status="409"} 2`, `live_ladder_starts_total{code="started"} 1`, `live_ladder_starts_total{code="encoder_init_failed"} 1`, `live_ladder_exits_total{code="process_killed"} 1`, "live_sessions_stalled_total 1", `live_gpu_telemetry_probes_total{result="available"} 1`} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q: %s", want, body)
		}
	}
	if recorder.Code != http.StatusOK || strings.Contains(body, "body_is_not_a_label") {
		t.Fatalf("metrics status=%d body=%q", recorder.Code, body)
	}
}

func callbackTestSessionV1(t *testing.T, callbackURL string) (*EncryptedFileSessionStoreV1, RunnerCreateRequestV1, RunnerCreateResponseV1) {
	t.Helper()
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x55}, 32))
	request, response := testCreatePairV1(t)
	request.CallbackURL = callbackURL
	if _, _, _, err := store.CreateOrReplay(request, response); err != nil {
		t.Fatal(err)
	}
	return store, request, response
}
