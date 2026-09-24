package liverunner

import (
	"context"
	"errors"
	"strconv"
	"time"
)

func RecoverLiveSessionsV1(ctx context.Context, store *EncryptedFileSessionStoreV1, runtime LiveSessionRuntimeV1, now func() time.Time) error {
	if store == nil || runtime == nil {
		return errors.New("live session recovery dependencies are incomplete")
	}
	if now == nil {
		now = time.Now
	}
	records, err := store.Recoverable()
	if err != nil {
		return err
	}
	for _, candidate := range records {
		if candidate.State != "active" {
			continue
		}
		record, secrets, err := store.Load(candidate.BrokerSessionID)
		if err != nil {
			return err
		}
		if record.Stopping {
			if _, err := CompleteLiveTerminationV1(ctx, store, runtime, record, now()); err != nil {
				return err
			}
			continue
		}
		if secrets == nil {
			return errors.New("active live session has no secrets")
		}
		if record.PendingKeyActivationID != "" {
			if err := runtime.ActivateStreamKey(ctx, record); err != nil {
				return err
			}
			if err := store.CompleteKeyActivation(record.BrokerSessionID, record.PendingKeyActivationID); err != nil {
				return err
			}
			record.PendingKeyActivationID = ""
		}
		if err := runtime.EnsureSession(ctx, record, *secrets); err != nil {
			return err
		}
	}
	return nil
}

func CompleteLiveTerminationV1(ctx context.Context, store *EncryptedFileSessionStoreV1, runtime LiveSessionRuntimeV1, record SessionRecordV1, now time.Time) (SessionRecordV1, error) {
	if store == nil || runtime == nil || record.State != "active" || !record.Stopping || !validCloseReasonV1(record.PendingCloseReason) {
		return SessionRecordV1{}, errors.New("live termination state is invalid")
	}
	if err := runtime.TerminateSession(ctx, record); err != nil {
		return SessionRecordV1{}, err
	}
	return FinalizeLiveTerminationV1(store, record.BrokerSessionID, now)
}

func FinalizeLiveTerminationV1(store *EncryptedFileSessionStoreV1, brokerSessionID string, now time.Time) (SessionRecordV1, error) {
	if store == nil || brokerSessionID == "" || now.IsZero() {
		return SessionRecordV1{}, errors.New("live termination finalization is invalid")
	}
	record, _, err := store.Load(brokerSessionID)
	if err != nil {
		return SessionRecordV1{}, errors.Join(errors.New("reload live termination state failed"), err)
	}
	if record.State == "ended" || record.State == "failed" {
		return record, nil
	}
	if record.State != "active" || !record.Stopping {
		return SessionRecordV1{}, errors.New("reload live termination state failed")
	}
	closeReason := record.PendingCloseReason
	terminalState := record.PendingTerminalState
	if terminalState == "" {
		terminalState = "ended"
	}
	eventType := "session.ended"
	if terminalState == "failed" {
		eventType = "session.failed"
	}
	details, err := outputHealthDetailsV1(record)
	if err != nil {
		return SessionRecordV1{}, err
	}
	event := RunnerEventV1{
		EventID: record.RunnerSessionID + ":" + strconv.FormatUint(record.LastSequence+1, 10), Sequence: record.LastSequence + 1,
		EventType: eventType, EventTime: now.UTC().Format(time.RFC3339Nano), State: terminalState,
		Usage: &UsageV1{Unit: WorkUnitV1, Total: record.UsageTotal}, CloseReason: &closeReason, Details: details,
	}
	if err := store.Advance(record.BrokerSessionID, event); err != nil {
		if errors.Is(err, ErrSessionTerminalV1) {
			completed, _, loadErr := store.Load(record.BrokerSessionID)
			return completed, loadErr
		}
		return SessionRecordV1{}, err
	}
	completed, _, err := store.Load(record.BrokerSessionID)
	return completed, err
}
