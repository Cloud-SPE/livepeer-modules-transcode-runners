package liverunner

import (
	"bytes"
	"context"
	"testing"
	"time"
)

type recoveryRuntimeV1 struct {
	ensured    []string
	terminated []string
	activated  []string
}

func (r *recoveryRuntimeV1) ValidateSession(RunnerCreateRequestV1) error { return nil }
func (r *recoveryRuntimeV1) EnsureSession(_ context.Context, record SessionRecordV1, _ SessionSecretsV1) error {
	r.ensured = append(r.ensured, record.BrokerSessionID)
	return nil
}
func (r *recoveryRuntimeV1) TerminateSession(_ context.Context, record SessionRecordV1) error {
	r.terminated = append(r.terminated, record.BrokerSessionID)
	return nil
}
func (r *recoveryRuntimeV1) ActivateStreamKey(_ context.Context, record SessionRecordV1) error {
	r.activated = append(r.activated, record.BrokerSessionID)
	return nil
}

func TestRecoverLiveSessionsConvergesActiveActivationAndStoppingState(t *testing.T) {
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x78}, 32))
	active, _ := createRuntimeSessionV1(t, store, "sess_recover_active", "runner_recover_active")
	pending, _ := createRuntimeSessionV1(t, store, "sess_recover_key", "runner_recover_key")
	firstRequest := StreamKeyIssueRequestV1{RequestID: "key_recover_001", Audience: "gateway-relay"}
	firstResponse := StreamKeyIssueResponseV1{RequestID: firstRequest.RequestID, StreamKey: "runner_recover_key?token=first-key", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	if _, _, err := store.RecordKeyIssue(pending.BrokerSessionID, firstRequest, firstResponse); err != nil {
		t.Fatal(err)
	}
	rotation := StreamKeyIssueRequestV1{RequestID: "key_recover_002", Audience: "gateway-relay"}
	rotationResponse := StreamKeyIssueResponseV1{RequestID: rotation.RequestID, StreamKey: "runner_recover_key?token=second-key", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	if _, _, err := store.RecordKeyIssue(pending.BrokerSessionID, rotation, rotationResponse); err != nil {
		t.Fatal(err)
	}
	stopping, _ := createRuntimeSessionV1(t, store, "sess_recover_stopping", "runner_recover_stopping")
	stopping, _, err := store.BeginTermination(stopping.BrokerSessionID, "gateway_close")
	if err != nil {
		t.Fatal(err)
	}
	runtime := &recoveryRuntimeV1{}
	fixedNow := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	if err := RecoverLiveSessionsV1(context.Background(), store, runtime, func() time.Time { return fixedNow }); err != nil {
		t.Fatal(err)
	}
	if len(runtime.ensured) != 2 || runtime.ensured[0] != active.BrokerSessionID || runtime.ensured[1] != pending.BrokerSessionID {
		t.Fatalf("ensured sessions=%v", runtime.ensured)
	}
	if len(runtime.activated) != 1 || runtime.activated[0] != pending.BrokerSessionID {
		t.Fatalf("activated sessions=%v", runtime.activated)
	}
	if len(runtime.terminated) != 1 || runtime.terminated[0] != stopping.BrokerSessionID {
		t.Fatalf("terminated sessions=%v", runtime.terminated)
	}
	recoveredPending, _, err := store.Load(pending.BrokerSessionID)
	if err != nil || recoveredPending.PendingKeyActivationID != "" {
		t.Fatalf("pending activation=%q err=%v", recoveredPending.PendingKeyActivationID, err)
	}
	recoveredStopping, _, err := store.Load(stopping.BrokerSessionID)
	if err != nil || recoveredStopping.State != "ended" || recoveredStopping.CloseReason != "gateway_close" || len(recoveredStopping.PendingEvents) != 1 {
		t.Fatalf("recovered stopping session=%+v err=%v", recoveredStopping, err)
	}
}

func TestCompleteLiveTerminationReportsDurableMeterTotal(t *testing.T) {
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x79}, 32))
	record, _ := createRuntimeSessionV1(t, store, "sess_terminate_meter", "runner_terminate_meter")
	if err := store.Advance(record.BrokerSessionID, testEventV1(record.RunnerSessionID, 1, "session.started", "active", 0, "")); err != nil {
		t.Fatal(err)
	}
	if emitted, err := store.RecordFinalizedSegments(record.BrokerSessionID, []FinalizedHLSSegmentV1{{URI: "final-segment.mp4", DurationMicroseconds: 1_250_000}}, time.Now()); err != nil || !emitted {
		t.Fatalf("meter emitted=%v err=%v", emitted, err)
	}
	record, _, err := store.BeginTermination(record.BrokerSessionID, "gateway_close")
	if err != nil {
		t.Fatal(err)
	}
	completed, err := CompleteLiveTerminationV1(context.Background(), store, &recoveryRuntimeV1{}, record, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	last := completed.PendingEvents[len(completed.PendingEvents)-1]
	if completed.State != "ended" || completed.UsageTotal != 1 || last.EventType != "session.ended" || last.Usage == nil || last.Usage.Total != 1 {
		t.Fatalf("completed session=%+v final=%+v", completed, last)
	}
}
