package liverunner

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestEncryptedStoreCreateReplayAndRestart(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{0x2a}, 32)
	store := newTestStoreV1(t, dir, key)
	request, response := testCreatePairV1(t)
	request.WorkID = "loc-auth:ce3fa091-844e-4004-b532-39d7d935061f"

	record, secrets, replay, err := store.CreateOrReplay(request, response)
	if err != nil || replay {
		t.Fatalf("create replay=%v err=%v", replay, err)
	}
	if record.BrokerSessionID != request.SessionID || secrets.CreateRequest.WorkID != request.WorkID || secrets.CreateRequest.CallbackToken != request.CallbackToken {
		t.Fatal("created state does not preserve session identity and private callback state")
	}
	statePath := filepath.Join(dir, request.SessionID+".json")
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode=%v", info.Mode().Perm())
	}
	onDisk, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"callback-secret-fixture", "grant-secret-fixture", "sts-secret-fixture", "sts-session-token-fixture"} {
		if bytes.Contains(onDisk, []byte(secret)) {
			t.Fatalf("state file contains plaintext secret %q", secret)
		}
	}

	restarted := newTestStoreV1(t, dir, key)
	replayedRecord, replayedSecrets, replay, err := restarted.CreateOrReplay(request, response)
	if err != nil || !replay || replayedRecord.RunnerSessionID != record.RunnerSessionID || replayedSecrets.CreateRequest.WorkID != request.WorkID || replayedSecrets.CreateResponse.Runtime.Grants[0].Secret != "grant-secret-fixture" {
		t.Fatalf("restart replay=%v record=%+v err=%v", replay, replayedRecord, err)
	}
	changed := request
	changed.CallbackToken = "changed"
	if _, _, _, err := restarted.CreateOrReplay(changed, response); !errors.Is(err, ErrSessionIDReuseV1) {
		t.Fatalf("changed create error=%v", err)
	}
	wrongKey := newTestStoreV1(t, dir, bytes.Repeat([]byte{0x3b}, 32))
	if _, _, err := wrongKey.Load(request.SessionID); err == nil {
		t.Fatal("wrong master key decrypted session state")
	}
}

func TestEncryptedStoreKeyRotationIsIdempotentAndSupersedesOldRequests(t *testing.T) {
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x4c}, 32))
	request, response := testCreatePairV1(t)
	if _, _, _, err := store.CreateOrReplay(request, response); err != nil {
		t.Fatal(err)
	}
	issue := readStrictFixtureV1[StreamKeyIssueRequestV1](t, "key-issue-request.json")
	issued := readStrictFixtureV1[StreamKeyIssueResponseV1](t, "key-issue-response.json")
	got, replay, err := store.RecordKeyIssue(request.SessionID, issue, issued)
	if err != nil || replay || got.StreamKey != issued.StreamKey {
		t.Fatalf("first issue replay=%v response=%+v err=%v", replay, got, err)
	}
	got, replay, err = store.RecordKeyIssue(request.SessionID, issue, issued)
	if err != nil || !replay || got != issued {
		t.Fatalf("issue replay=%v response=%+v err=%v", replay, got, err)
	}
	changed := issue
	changed.Audience = "direct-publisher"
	if _, _, err := store.RecordKeyIssue(request.SessionID, changed, issued); !errors.Is(err, ErrRequestIDReuseV1) {
		t.Fatalf("changed issue error=%v", err)
	}
	rotatedRequest := StreamKeyIssueRequestV1{RequestID: "key_issue_002", Audience: issue.Audience}
	rotatedResponse := StreamKeyIssueResponseV1{RequestID: rotatedRequest.RequestID, StreamKey: "rotated-stream-key", ExpiresAt: "2030-01-01T00:00:00Z"}
	if _, replay, err := store.RecordKeyIssue(request.SessionID, rotatedRequest, rotatedResponse); err != nil || replay {
		t.Fatalf("rotation replay=%v err=%v", replay, err)
	}
	if _, _, err := store.RecordKeyIssue(request.SessionID, issue, issued); !errors.Is(err, ErrRequestSupersededV1) {
		t.Fatalf("superseded issue error=%v", err)
	}
	_, secrets, err := store.Load(request.SessionID)
	if err != nil || secrets == nil || secrets.KeyIssues[issue.RequestID].Response.StreamKey != "" || secrets.KeyIssues[rotatedRequest.RequestID].Response.StreamKey != rotatedResponse.StreamKey {
		t.Fatalf("rotated credential retention=%+v err=%v", secrets, err)
	}
}

func TestEncryptedStoreEventOutboxRecoveryAndCryptoErase(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{0x5d}, 32)
	store := newTestStoreV1(t, dir, key)
	request, response := testCreatePairV1(t)
	if _, _, _, err := store.CreateOrReplay(request, response); err != nil {
		t.Fatal(err)
	}
	started := testEventV1(response.RunnerSessionID, 1, "session.started", "active", 0, "")
	if err := store.Advance(request.SessionID, started); err != nil {
		t.Fatal(err)
	}
	if err := store.Advance(request.SessionID, started); err == nil {
		t.Fatal("duplicate event was accepted")
	}
	if err := store.AcknowledgeEvent(request.SessionID, "wrong"); err == nil {
		t.Fatal("out-of-order acknowledgement was accepted")
	}

	restarted := newTestStoreV1(t, dir, key)
	recoverable, err := restarted.Recoverable()
	if err != nil || len(recoverable) != 1 || len(recoverable[0].PendingEvents) != 1 {
		t.Fatalf("recoverable=%+v err=%v", recoverable, err)
	}
	if err := restarted.AcknowledgeEvent(request.SessionID, started.EventID); err != nil {
		t.Fatal(err)
	}
	ended := testEventV1(response.RunnerSessionID, 2, "session.ended", "ended", 17, "gateway_close")
	if err := restarted.Advance(request.SessionID, ended); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ClearTerminalSecrets(request.SessionID); err == nil {
		t.Fatal("terminal secrets cleared before final callback acknowledgement")
	}
	if err := restarted.AcknowledgeEvent(request.SessionID, ended.EventID); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ClearTerminalSecrets(request.SessionID); err != nil {
		t.Fatal(err)
	}
	record, secrets, err := restarted.Load(request.SessionID)
	if err != nil || secrets != nil || record.State != "ended" || record.UsageTotal != 17 {
		t.Fatalf("terminal state=%+v secrets=%+v err=%v", record, secrets, err)
	}
	if record.WrappedKeyCiphertext != "" || record.SecretCiphertext != "" {
		t.Fatal("terminal state retained a decryptable secret envelope")
	}
	recoverable, err = restarted.Recoverable()
	if err != nil || len(recoverable) != 0 {
		t.Fatalf("cleared terminal session remained recoverable: %+v err=%v", recoverable, err)
	}
}

func TestEncryptedStoreMetersFinalizedSegmentsExactlyOnceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{0x5e}, 32)
	store := newTestStoreV1(t, dir, key)
	request, response := testCreatePairV1(t)
	if _, _, _, err := store.CreateOrReplay(request, response); err != nil {
		t.Fatal(err)
	}
	if err := store.Advance(request.SessionID, testEventV1(response.RunnerSessionID, 1, "session.started", "active", 0, "")); err != nil {
		t.Fatal(err)
	}
	first := []FinalizedHLSSegmentV1{{URI: "seg-1.mp4", DurationMicroseconds: 400_000}, {URI: "seg-2.mp4", DurationMicroseconds: 600_000}}
	emitted, err := store.RecordFinalizedSegments(request.SessionID, first, time.Date(2026, 8, 24, 12, 0, 1, 0, time.UTC))
	if err != nil || !emitted {
		t.Fatalf("first emitted=%v err=%v", emitted, err)
	}
	record, _, err := store.Load(request.SessionID)
	if err != nil || record.MeteredMicroseconds != 1_000_000 || record.UsageTotal != 1 || record.LastSequence != 2 || len(record.MeteredSegmentSHA256) != 2 || len(record.PendingEvents) != 2 {
		t.Fatalf("first record=%+v err=%v", record, err)
	}

	restarted := newTestStoreV1(t, dir, key)
	emitted, err = restarted.RecordFinalizedSegments(request.SessionID, append(first, FinalizedHLSSegmentV1{URI: "seg-3.mp4", DurationMicroseconds: 500_000}), time.Date(2026, 8, 24, 12, 0, 2, 0, time.UTC))
	if err != nil || emitted {
		t.Fatalf("fraction emitted=%v err=%v", emitted, err)
	}
	emitted, err = restarted.RecordFinalizedSegments(request.SessionID, []FinalizedHLSSegmentV1{{URI: "seg-3.mp4", DurationMicroseconds: 500_000}, {URI: "seg-4.mp4", DurationMicroseconds: 500_000}}, time.Date(2026, 8, 24, 12, 0, 3, 0, time.UTC))
	if err != nil || !emitted {
		t.Fatalf("second emitted=%v err=%v", emitted, err)
	}
	record, _, err = restarted.Load(request.SessionID)
	if err != nil || record.MeteredMicroseconds != 2_000_000 || record.UsageTotal != 2 || record.LastSequence != 3 || len(record.MeteredSegmentSHA256) != 4 || len(record.PendingEvents) != 3 {
		t.Fatalf("restarted record=%+v err=%v", record, err)
	}
	last := record.PendingEvents[len(record.PendingEvents)-1]
	if last.EventID != response.RunnerSessionID+":3" || last.EventType != "session.usage.tick" || last.Usage == nil || last.Usage.Total != 2 {
		t.Fatalf("usage event=%+v", last)
	}
}

func TestEncryptedStoreHeartbeatUsesDurableEventCadence(t *testing.T) {
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x60}, 32))
	request, response := testCreatePairV1(t)
	if _, _, _, err := store.CreateOrReplay(request, response); err != nil {
		t.Fatal(err)
	}
	started := testEventV1(response.RunnerSessionID, 1, "session.started", "active", 0, "")
	if err := store.Advance(request.SessionID, started); err != nil {
		t.Fatal(err)
	}
	startedAt, _ := time.Parse(time.RFC3339Nano, started.EventTime)
	if emitted, err := store.RecordHeartbeat(request.SessionID, startedAt.Add(4*time.Second), 5*time.Second); err != nil || emitted {
		t.Fatalf("early heartbeat emitted=%v err=%v", emitted, err)
	}
	if emitted, err := store.RecordHeartbeat(request.SessionID, startedAt.Add(5*time.Second), 5*time.Second); err != nil || !emitted {
		t.Fatalf("due heartbeat emitted=%v err=%v", emitted, err)
	}
	record, _, err := store.Load(request.SessionID)
	if err != nil || record.LastSequence != 2 || len(record.PendingEvents) != 2 || record.PendingEvents[1].EventType != "session.heartbeat" || record.PendingEvents[1].Usage == nil || record.PendingEvents[1].Usage.Total != 0 {
		t.Fatalf("heartbeat record=%+v err=%v", record, err)
	}
}

func TestEncryptedStorePersistsOutputHealthAndFailedTerminalAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{0x61}, 32)
	store := newTestStoreV1(t, dir, key)
	request, response := testCreatePairV1(t)
	if _, _, _, err := store.CreateOrReplay(request, response); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	started := testEventV1(response.RunnerSessionID, 1, "session.started", "active", 0, "")
	started.EventTime = startedAt.Format(time.RFC3339Nano)
	if err := store.Advance(request.SessionID, started); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordIngestPresence(request.SessionID, true, startedAt); err != nil {
		t.Fatal(err)
	}
	if failed, stalled, err := store.EvaluateOutputHealth(request.SessionID, startedAt.Add(20*time.Second), 20*time.Second, time.Minute); err != nil || failed || !stalled {
		t.Fatalf("stall evaluation failed=%v stalled=%v err=%v", failed, stalled, err)
	}
	restarted := newTestStoreV1(t, dir, key)
	record, _, err := restarted.Load(request.SessionID)
	if err != nil || record.OutputState != OutputStateStalledV1 || record.IngestOnlineAt == "" || len(record.PendingEvents) != 2 || record.PendingEvents[1].EventType != "session.output.stalled" {
		t.Fatalf("stalled record=%+v err=%v", record, err)
	}
	if failed, stalled, err := restarted.EvaluateOutputHealth(request.SessionID, startedAt.Add(time.Minute), 20*time.Second, time.Minute); err != nil || !failed || stalled {
		t.Fatalf("failure deadline failed=%v stalled=%v err=%v", failed, stalled, err)
	}
	stopping, began, err := restarted.BeginFailure(request.SessionID, "output_failed")
	if err != nil || !began || stopping.PendingTerminalState != "failed" {
		t.Fatalf("begin failure began=%v record=%+v err=%v", began, stopping, err)
	}
	final, err := FinalizeLiveTerminationV1(restarted, request.SessionID, startedAt.Add(time.Minute))
	if err != nil || final.State != "failed" || final.CloseReason != "output_failed" || final.PendingEvents[len(final.PendingEvents)-1].EventType != "session.failed" {
		t.Fatalf("failed terminal=%+v err=%v", final, err)
	}
}

func TestEncryptedStoreLadderRestartWindowResetsAfterOutput(t *testing.T) {
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x62}, 32))
	request, response := testCreatePairV1(t)
	if _, _, _, err := store.CreateOrReplay(request, response); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	started := testEventV1(response.RunnerSessionID, 1, "session.started", "active", 0, "")
	started.EventTime = startedAt.Format(time.RFC3339Nano)
	if err := store.Advance(request.SessionID, started); err != nil {
		t.Fatal(err)
	}
	if attempt, err := store.RecordLadderRestart(request.SessionID, "encoder_init_failed", startedAt.Add(time.Second), time.Minute); err != nil || attempt != 1 {
		t.Fatalf("first restart attempt=%d err=%v", attempt, err)
	}
	if _, err := store.RecordFinalizedSegments(request.SessionID, []FinalizedHLSSegmentV1{{URI: "segment.mp4", DurationMicroseconds: 1_000_000}}, startedAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if attempt, err := store.RecordLadderRestart(request.SessionID, "unknown", startedAt.Add(3*time.Second), time.Minute); err != nil || attempt != 1 {
		t.Fatalf("restart after output attempt=%d err=%v", attempt, err)
	}
	record, _, err := store.Load(request.SessionID)
	if err != nil || record.FirstFinalizedSegmentAt == "" || record.LastFinalizedSegmentAt == "" || record.OutputState != OutputStateProducingV1 || record.LastLadderFailureCode != "unknown" {
		t.Fatalf("output health record=%+v err=%v", record, err)
	}
}

func TestEncryptedStoreRejectsTamperedState(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreV1(t, dir, bytes.Repeat([]byte{0x6e}, 32))
	request, response := testCreatePairV1(t)
	if _, _, _, err := store.CreateOrReplay(request, response); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, request.SessionID+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record SessionRecordV1
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	if record.SecretCiphertext[0] == '0' {
		record.SecretCiphertext = "1" + record.SecretCiphertext[1:]
	} else {
		record.SecretCiphertext = "0" + record.SecretCiphertext[1:]
	}
	body, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load(request.SessionID); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}

func TestEncryptedStoreRejectsTamperedPublicMetadata(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreV1(t, dir, bytes.Repeat([]byte{0x6f}, 32))
	request, response := testCreatePairV1(t)
	if _, _, _, err := store.CreateOrReplay(request, response); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, request.SessionID+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record SessionRecordV1
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	record.RuntimePublic.HLSURL = "https://attacker.example/master.m3u8"
	body, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load(request.SessionID); err == nil {
		t.Fatal("tampered public metadata was accepted")
	}
}

func TestEncryptedStoreRequiresDeterministicEventIDs(t *testing.T) {
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x7f}, 32))
	request, response := testCreatePairV1(t)
	if _, _, _, err := store.CreateOrReplay(request, response); err != nil {
		t.Fatal(err)
	}
	event := testEventV1(response.RunnerSessionID, 1, "session.started", "active", 0, "")
	event.EventID = "arbitrary_event_id"
	if err := store.Advance(request.SessionID, event); err == nil {
		t.Fatal("non-deterministic event ID was accepted")
	}
}

func TestEncryptedStorePinsTerminationReasonBeforeTerminalEvent(t *testing.T) {
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x71}, 32))
	request, response := testCreatePairV1(t)
	if _, _, _, err := store.CreateOrReplay(request, response); err != nil {
		t.Fatal(err)
	}
	record, began, err := store.BeginTermination(request.SessionID, "gateway_close")
	if err != nil || !began || !record.Stopping || record.PendingCloseReason != "gateway_close" {
		t.Fatalf("begin termination began=%v record=%+v err=%v", began, record, err)
	}
	if _, began, err := store.BeginTermination(request.SessionID, "runner_failed"); err != nil || began {
		t.Fatalf("repeated termination began=%v err=%v", began, err)
	}
	mismatch := testEventV1(response.RunnerSessionID, 1, "session.ended", "ended", 0, "runner_failed")
	if err := store.Advance(request.SessionID, mismatch); err == nil {
		t.Fatal("terminal event changed the durable close reason")
	}
	terminal := testEventV1(response.RunnerSessionID, 1, "session.ended", "ended", 0, "gateway_close")
	if err := store.Advance(request.SessionID, terminal); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptedStoreBoundsCallbackDeadLettersWithoutLosingTotal(t *testing.T) {
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x55}, 32))
	request, response := testCreatePairV1(t)
	if _, _, _, err := store.CreateOrReplay(request, response); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for sequence := uint64(1); sequence <= uint64(maxCallbackDeadLettersV1+2); sequence++ {
		event := testEventV1(response.RunnerSessionID, sequence, "session.heartbeat", "active", 0, "")
		if err := store.Advance(request.SessionID, event); err != nil {
			t.Fatal(err)
		}
		at := base.Add(time.Duration(sequence) * time.Second)
		if _, err := store.ReserveCallbackAttempt(request.SessionID, event.EventID, at, time.Millisecond, time.Second); err != nil {
			t.Fatal(err)
		}
		if err := store.ParkCallbackRejection(request.SessionID, event.EventID, http.StatusConflict, "http_409", at); err != nil {
			t.Fatal(err)
		}
	}
	record, _, err := store.Load(request.SessionID)
	if err != nil || len(record.CallbackDeadLetters) != maxCallbackDeadLettersV1 || record.CallbackRejectedTotal != uint64(maxCallbackDeadLettersV1+2) {
		t.Fatalf("dead letters=%d total=%d err=%v", len(record.CallbackDeadLetters), record.CallbackRejectedTotal, err)
	}
	if record.CallbackDeadLetters[0].Event.Sequence != 3 {
		t.Fatalf("oldest retained callback sequence=%d", record.CallbackDeadLetters[0].Event.Sequence)
	}
}

func newTestStoreV1(t *testing.T, dir string, key []byte) *EncryptedFileSessionStoreV1 {
	t.Helper()
	store, err := NewEncryptedFileSessionStoreV1(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testCreatePairV1(t *testing.T) (RunnerCreateRequestV1, RunnerCreateResponseV1) {
	t.Helper()
	return readStrictFixtureV1[RunnerCreateRequestV1](t, "create-request.json"), readStrictFixtureV1[RunnerCreateResponseV1](t, "create-response.json")
}

func testEventV1(runnerSessionID string, sequence uint64, eventType, state string, total uint64, closeReason string) RunnerEventV1 {
	event := RunnerEventV1{
		EventID: runnerSessionID + ":" + strconv.FormatUint(sequence, 10), Sequence: sequence,
		EventType: eventType, EventTime: time.Date(2026, 8, 23, 12, 0, int(sequence), 0, time.UTC).Format(time.RFC3339Nano),
		State: state, Details: json.RawMessage(`{}`),
	}
	if eventType == "session.usage.tick" || eventType == "session.ended" || eventType == "session.failed" {
		event.Usage = &UsageV1{Unit: WorkUnitV1, Total: total}
	}
	if closeReason != "" {
		event.CloseReason = &closeReason
	}
	return event
}
