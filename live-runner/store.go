package liverunner

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"
)

const sessionRecordVersionV1 = 1

const maxCallbackDeadLettersV1 = 32

var (
	ErrSessionIDReuseV1        = errors.New("runner_session_id_reuse")
	ErrRequestIDReuseV1        = errors.New("request_id_reuse")
	ErrRequestSupersededV1     = errors.New("request_id_superseded")
	ErrKeyActivationInFlightV1 = errors.New("key_activation_in_flight")
	ErrSessionTerminalV1       = errors.New("session_terminal")
	ErrCallbackNotDueV1        = errors.New("callback_not_due")
)

type GrantAuditV1 struct {
	ID           string   `json:"id"`
	Operations   []string `json:"operations"`
	SecretSHA256 string   `json:"secret_sha256"`
	ExpiresAt    string   `json:"expires_at"`
}

type CallbackDeliveryAttemptV1 struct {
	EventID        string `json:"event_id"`
	Attempts       uint32 `json:"attempts"`
	LastAttemptAt  string `json:"last_attempt_at"`
	NextAttemptAt  string `json:"next_attempt_at"`
	LastStatusCode int    `json:"last_status_code,omitempty"`
	LastErrorCode  string `json:"last_error_code,omitempty"`
}

type CallbackDeadLetterV1 struct {
	Event      RunnerEventV1 `json:"event"`
	Attempts   uint32        `json:"attempts"`
	StatusCode int           `json:"status_code"`
	ErrorCode  string        `json:"error_code"`
	RejectedAt string        `json:"rejected_at"`
}

type SessionRecordV1 struct {
	Version                   int                        `json:"version"`
	BrokerSessionID           string                     `json:"broker_session_id"`
	RunnerSessionID           string                     `json:"runner_session_id"`
	CreateFingerprint         string                     `json:"create_fingerprint"`
	State                     string                     `json:"state"`
	Stopping                  bool                       `json:"stopping,omitempty"`
	PendingCloseReason        string                     `json:"pending_close_reason,omitempty"`
	PendingTerminalState      string                     `json:"pending_terminal_state,omitempty"`
	PendingKeyActivationID    string                     `json:"pending_key_activation_id,omitempty"`
	RuntimePublic             RuntimePublicV1            `json:"runtime_public"`
	GrantAudit                GrantAuditV1               `json:"grant_audit"`
	UsageTotal                uint64                     `json:"usage_total"`
	LastSequence              uint64                     `json:"last_sequence"`
	MeteredMicroseconds       uint64                     `json:"metered_microseconds,omitempty"`
	MeteredSegmentSHA256      []string                   `json:"metered_segment_sha256,omitempty"`
	IngestOnlineAt            string                     `json:"ingest_online_at,omitempty"`
	FirstFinalizedSegmentAt   string                     `json:"first_finalized_segment_at,omitempty"`
	LastFinalizedSegmentAt    string                     `json:"last_finalized_segment_at,omitempty"`
	OutputState               string                     `json:"output_state"`
	OutputStateSince          string                     `json:"output_state_since"`
	LastLadderFailureCode     string                     `json:"last_ladder_failure_code,omitempty"`
	LadderFailureWindowAt     string                     `json:"ladder_failure_window_at,omitempty"`
	LadderConsecutiveFailures uint32                     `json:"ladder_consecutive_failures,omitempty"`
	LastEventAt               string                     `json:"last_event_at,omitempty"`
	PendingEvents             []RunnerEventV1            `json:"pending_events"`
	CallbackDelivery          *CallbackDeliveryAttemptV1 `json:"callback_delivery,omitempty"`
	CallbackDeadLetters       []CallbackDeadLetterV1     `json:"callback_dead_letters,omitempty"`
	CallbackRejectedTotal     uint64                     `json:"callback_rejected_total,omitempty"`
	CloseReason               string                     `json:"close_reason,omitempty"`
	IntegritySHA256           string                     `json:"integrity_sha256"`
	WrappedKeyNonce           string                     `json:"wrapped_key_nonce,omitempty"`
	WrappedKeyCiphertext      string                     `json:"wrapped_key_ciphertext,omitempty"`
	SecretNonce               string                     `json:"secret_nonce,omitempty"`
	SecretCiphertext          string                     `json:"secret_ciphertext,omitempty"`
}

type SessionSecretsV1 struct {
	CreateRequest  RunnerCreateRequestV1       `json:"create_request"`
	CreateResponse RunnerCreateResponseV1      `json:"create_response"`
	CurrentKeyID   string                      `json:"current_key_request_id,omitempty"`
	KeyIssues      map[string]StoredKeyIssueV1 `json:"key_issues"`
}

type StoredKeyIssueV1 struct {
	Fingerprint string                   `json:"fingerprint"`
	Request     StreamKeyIssueRequestV1  `json:"request"`
	Response    StreamKeyIssueResponseV1 `json:"response"`
}

type EncryptedFileSessionStoreV1 struct {
	dir  string
	aead cipher.AEAD
	key  []byte
	mu   sync.Mutex
}

func NewEncryptedFileSessionStoreV1(dir string, key []byte) (*EncryptedFileSessionStoreV1, error) {
	if dir == "" || len(key) != 32 {
		return nil, errors.New("state directory and 32-byte encryption key are required")
	}
	encryptionKey := deriveKeyV1(key, "session-store/encryption/v1")
	integrityKey := deriveKeyV1(key, "session-store/integrity/v1")
	aead, err := newAESGCMV1(encryptionKey)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create session state directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure session state directory: %w", err)
	}
	return &EncryptedFileSessionStoreV1{dir: dir, aead: aead, key: integrityKey}, nil
}

func (s *EncryptedFileSessionStoreV1) CreateOrReplay(request RunnerCreateRequestV1, response RunnerCreateResponseV1) (SessionRecordV1, SessionSecretsV1, bool, error) {
	if err := ValidateCreateRequestV1(request); err != nil {
		return SessionRecordV1{}, SessionSecretsV1{}, false, err
	}
	if err := ValidateCreateResponseV1(response); err != nil {
		return SessionRecordV1{}, SessionSecretsV1{}, false, err
	}
	fingerprint, err := CreateFingerprintV1(request)
	if err != nil {
		return SessionRecordV1{}, SessionSecretsV1{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, secrets, err := s.loadLocked(request.SessionID)
	if err == nil {
		if record.CreateFingerprint != fingerprint {
			return SessionRecordV1{}, SessionSecretsV1{}, false, ErrSessionIDReuseV1
		}
		if record.State != "active" || record.Stopping || secrets == nil {
			return SessionRecordV1{}, SessionSecretsV1{}, false, ErrSessionTerminalV1
		}
		return cloneSessionRecordV1(record), cloneSessionSecretsV1(*secrets), true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return SessionRecordV1{}, SessionSecretsV1{}, false, err
	}
	grant := response.Runtime.Grants[0]
	record = SessionRecordV1{
		Version: sessionRecordVersionV1, BrokerSessionID: request.SessionID,
		RunnerSessionID: response.RunnerSessionID, CreateFingerprint: fingerprint,
		State: "active", RuntimePublic: response.Runtime.Public,
		OutputState: OutputStateWaitingV1, OutputStateSince: time.Now().UTC().Format(time.RFC3339Nano),
		GrantAudit:    GrantAuditV1{ID: grant.ID, Operations: append([]string(nil), grant.Operations...), SecretSHA256: secretSHA256V1(grant.Secret), ExpiresAt: grant.ExpiresAt},
		PendingEvents: []RunnerEventV1{},
	}
	secretState := SessionSecretsV1{CreateRequest: request, CreateResponse: response, KeyIssues: map[string]StoredKeyIssueV1{}}
	if err := s.sealLocked(&record, secretState); err != nil {
		return SessionRecordV1{}, SessionSecretsV1{}, false, err
	}
	if err := s.saveLocked(record); err != nil {
		return SessionRecordV1{}, SessionSecretsV1{}, false, err
	}
	return cloneSessionRecordV1(record), cloneSessionSecretsV1(secretState), false, nil
}

func (s *EncryptedFileSessionStoreV1) Load(brokerSessionID string) (SessionRecordV1, *SessionSecretsV1, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, secrets, err := s.loadLocked(brokerSessionID)
	if err != nil {
		return SessionRecordV1{}, nil, err
	}
	copy := cloneSessionRecordV1(record)
	if secrets == nil {
		return copy, nil, nil
	}
	secretCopy := cloneSessionSecretsV1(*secrets)
	return copy, &secretCopy, nil
}

func (s *EncryptedFileSessionStoreV1) RecordKeyIssue(brokerSessionID string, request StreamKeyIssueRequestV1, response StreamKeyIssueResponseV1) (StreamKeyIssueResponseV1, bool, error) {
	if err := ValidateStreamKeyIssueRequestV1(request); err != nil {
		return StreamKeyIssueResponseV1{}, false, err
	}
	if err := ValidateStreamKeyIssueResponseV1(request, response); err != nil {
		return StreamKeyIssueResponseV1{}, false, err
	}
	fingerprint, err := KeyIssueFingerprintV1(request)
	if err != nil {
		return StreamKeyIssueResponseV1{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, secrets, err := s.loadLocked(brokerSessionID)
	if err != nil {
		return StreamKeyIssueResponseV1{}, false, err
	}
	if record.State != "active" || record.Stopping || secrets == nil {
		return StreamKeyIssueResponseV1{}, false, ErrSessionTerminalV1
	}
	if stored, ok := secrets.KeyIssues[request.RequestID]; ok {
		if stored.Fingerprint != fingerprint {
			return StreamKeyIssueResponseV1{}, false, ErrRequestIDReuseV1
		}
		if secrets.CurrentKeyID != request.RequestID {
			return StreamKeyIssueResponseV1{}, false, ErrRequestSupersededV1
		}
		return stored.Response, true, nil
	}
	if record.PendingKeyActivationID != "" {
		return StreamKeyIssueResponseV1{}, false, ErrKeyActivationInFlightV1
	}
	for id, stored := range secrets.KeyIssues {
		stored.Response.StreamKey = ""
		secrets.KeyIssues[id] = stored
	}
	if secrets.CurrentKeyID != "" {
		record.PendingKeyActivationID = request.RequestID
	}
	secrets.KeyIssues[request.RequestID] = StoredKeyIssueV1{Fingerprint: fingerprint, Request: request, Response: response}
	secrets.CurrentKeyID = request.RequestID
	if err := s.sealLocked(&record, *secrets); err != nil {
		return StreamKeyIssueResponseV1{}, false, err
	}
	if err := s.saveLocked(record); err != nil {
		return StreamKeyIssueResponseV1{}, false, err
	}
	return response, false, nil
}

func (s *EncryptedFileSessionStoreV1) CompleteKeyActivation(brokerSessionID, requestID string) error {
	if !opaqueIDPattern.MatchString(requestID) {
		return errors.New("key activation request ID is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, secrets, err := s.loadLocked(brokerSessionID)
	if err != nil {
		return err
	}
	if record.State != "active" || record.Stopping || secrets == nil {
		return ErrSessionTerminalV1
	}
	if record.PendingKeyActivationID == "" {
		return nil
	}
	if record.PendingKeyActivationID != requestID || secrets.CurrentKeyID != requestID {
		return errors.New("key activation identity mismatch")
	}
	record.PendingKeyActivationID = ""
	return s.saveLocked(record)
}

func (s *EncryptedFileSessionStoreV1) Advance(brokerSessionID string, event RunnerEventV1) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, _, err := s.loadLocked(brokerSessionID)
	if err != nil {
		return err
	}
	if record.State != "active" || (record.Stopping && event.State == "active") {
		return ErrSessionTerminalV1
	}
	if record.Stopping && (event.CloseReason == nil || *event.CloseReason != record.PendingCloseReason) {
		return errors.New("terminal event does not match durable close reason")
	}
	if event.EventID != record.RunnerSessionID+":"+strconv.FormatUint(event.Sequence, 10) {
		return errors.New("event ID must be derived from runner session ID and sequence")
	}
	if (record.MeteredMicroseconds > 0 || len(record.MeteredSegmentSHA256) > 0) && event.Usage != nil && event.Usage.Total != record.MeteredMicroseconds/1_000_000 {
		return errors.New("event usage does not match the durable meter")
	}
	cursor := EventCursorV1{Sequence: record.LastSequence, UsageTotal: record.UsageTotal, HasUsage: record.LastSequence > 0, SeenIDs: make(map[string]struct{})}
	for _, pending := range record.PendingEvents {
		cursor.SeenIDs[pending.EventID] = struct{}{}
	}
	if err := cursor.Accept(event); err != nil {
		return err
	}
	record.LastSequence = cursor.Sequence
	record.UsageTotal = cursor.UsageTotal
	record.LastEventAt = event.EventTime
	record.PendingEvents = append(record.PendingEvents, event)
	if event.State == "ended" || event.State == "failed" {
		record.State = event.State
		record.CloseReason = *event.CloseReason
		record.Stopping = false
		record.PendingCloseReason = ""
		record.PendingTerminalState = ""
		record.PendingKeyActivationID = ""
	}
	return s.saveLocked(record)
}

// RecordFinalizedSegments persists the exactly-once segment cursor, fractional
// duration, and any newly earned whole-second usage event in one file replace.
// The caller may safely replay any playlist after a timeout or process restart.
func (s *EncryptedFileSessionStoreV1) RecordFinalizedSegments(brokerSessionID string, segments []FinalizedHLSSegmentV1, eventTime time.Time) (bool, error) {
	if eventTime.IsZero() {
		return false, errors.New("metering event time is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, _, err := s.loadLocked(brokerSessionID)
	if err != nil {
		return false, err
	}
	if record.State != "active" || record.Stopping {
		return false, ErrSessionTerminalV1
	}
	if record.LastSequence == 0 {
		return false, errors.New("session start must be recorded before usage")
	}
	seen := make(map[string]struct{}, len(record.MeteredSegmentSHA256)+len(segments))
	for _, identity := range record.MeteredSegmentSHA256 {
		seen[identity] = struct{}{}
	}
	changed := false
	for _, segment := range segments {
		if segment.URI == "" || segment.DurationMicroseconds == 0 {
			return false, errors.New("finalized HLS segment is invalid")
		}
		digest := sha256.Sum256([]byte(segment.URI))
		identity := hex.EncodeToString(digest[:])
		if _, exists := seen[identity]; exists {
			continue
		}
		total, carry := bits.Add64(record.MeteredMicroseconds, segment.DurationMicroseconds, 0)
		if carry != 0 {
			return false, errors.New("output timeline overflow")
		}
		record.MeteredMicroseconds = total
		record.MeteredSegmentSHA256 = append(record.MeteredSegmentSHA256, identity)
		seen[identity] = struct{}{}
		changed = true
	}
	if !changed {
		return false, nil
	}
	stamp := eventTime.UTC().Format(time.RFC3339Nano)
	if record.FirstFinalizedSegmentAt == "" {
		record.FirstFinalizedSegmentAt = stamp
	}
	record.LastFinalizedSegmentAt = stamp
	if record.OutputState != OutputStateProducingV1 {
		record.OutputState = OutputStateProducingV1
		record.OutputStateSince = stamp
	}
	record.LadderFailureWindowAt = ""
	record.LadderConsecutiveFailures = 0
	usageTotal := record.MeteredMicroseconds / 1_000_000
	emitted := usageTotal > record.UsageTotal
	if emitted {
		if record.LastSequence == ^uint64(0) {
			return false, errors.New("event sequence overflow")
		}
		sequence := record.LastSequence + 1
		details, err := outputHealthDetailsV1(record)
		if err != nil {
			return false, err
		}
		event := RunnerEventV1{
			EventID: record.RunnerSessionID + ":" + strconv.FormatUint(sequence, 10), Sequence: sequence,
			EventType: "session.usage.tick", EventTime: eventTime.UTC().Format(time.RFC3339Nano), State: "active",
			Usage: &UsageV1{Unit: WorkUnitV1, Total: usageTotal}, Details: details,
		}
		if err := ValidateEventV1(event); err != nil {
			return false, err
		}
		record.LastSequence = sequence
		record.UsageTotal = usageTotal
		record.LastEventAt = event.EventTime
		record.PendingEvents = append(record.PendingEvents, event)
	}
	return emitted, s.saveLocked(record)
}

func (s *EncryptedFileSessionStoreV1) RecordHeartbeat(brokerSessionID string, eventTime time.Time, minimumInterval time.Duration) (bool, error) {
	if eventTime.IsZero() || minimumInterval <= 0 {
		return false, errors.New("heartbeat time and interval are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, _, err := s.loadLocked(brokerSessionID)
	if err != nil {
		return false, err
	}
	if record.State != "active" || record.Stopping {
		return false, ErrSessionTerminalV1
	}
	if record.LastSequence == 0 {
		return false, errors.New("session start must be recorded before heartbeat")
	}
	if record.LastEventAt != "" {
		last, err := time.Parse(time.RFC3339Nano, record.LastEventAt)
		if err != nil {
			return false, errors.New("durable event time is invalid")
		}
		if eventTime.Before(last.Add(minimumInterval)) {
			return false, nil
		}
	}
	if record.LastSequence == ^uint64(0) {
		return false, errors.New("event sequence overflow")
	}
	sequence := record.LastSequence + 1
	details, err := outputHealthDetailsV1(record)
	if err != nil {
		return false, err
	}
	event := RunnerEventV1{
		EventID: record.RunnerSessionID + ":" + strconv.FormatUint(sequence, 10), Sequence: sequence,
		EventType: "session.heartbeat", EventTime: eventTime.UTC().Format(time.RFC3339Nano), State: "active",
		Usage: &UsageV1{Unit: WorkUnitV1, Total: record.UsageTotal}, Details: details,
	}
	if err := ValidateEventV1(event); err != nil {
		return false, err
	}
	record.LastSequence = sequence
	record.LastEventAt = event.EventTime
	record.PendingEvents = append(record.PendingEvents, event)
	return true, s.saveLocked(record)
}

// RecordIngestPresence maintains the current publisher epoch. Reconnects get
// a fresh output deadline; historical finalized-segment timestamps remain for
// audit and metering recovery.
func (s *EncryptedFileSessionStoreV1) RecordIngestPresence(brokerSessionID string, online bool, eventTime time.Time) error {
	if eventTime.IsZero() {
		return errors.New("ingest observation time is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, _, err := s.loadLocked(brokerSessionID)
	if err != nil {
		return err
	}
	if record.State != "active" || record.Stopping {
		return ErrSessionTerminalV1
	}
	stamp := eventTime.UTC().Format(time.RFC3339Nano)
	changed := false
	if online && record.IngestOnlineAt == "" {
		record.IngestOnlineAt = stamp
		record.OutputState = OutputStateWaitingV1
		record.OutputStateSince = stamp
		changed = true
	}
	if !online && record.IngestOnlineAt != "" {
		record.IngestOnlineAt = ""
		record.OutputState = OutputStateWaitingV1
		record.OutputStateSince = stamp
		record.LadderFailureWindowAt = ""
		record.LadderConsecutiveFailures = 0
		changed = true
	}
	if !changed {
		return nil
	}
	return s.saveLocked(record)
}

func (s *EncryptedFileSessionStoreV1) RecordLadderRestart(brokerSessionID, code string, eventTime time.Time, window time.Duration) (uint32, error) {
	if !validLadderFailureCodeV1(code, false) || eventTime.IsZero() || window <= 0 {
		return 0, errors.New("ladder restart observation is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, _, err := s.loadLocked(brokerSessionID)
	if err != nil {
		return 0, err
	}
	if record.State != "active" || record.Stopping {
		return 0, ErrSessionTerminalV1
	}
	now := eventTime.UTC()
	windowAt, _ := time.Parse(time.RFC3339Nano, record.LadderFailureWindowAt)
	if windowAt.IsZero() || now.Sub(windowAt) > window {
		record.LadderFailureWindowAt = now.Format(time.RFC3339Nano)
		record.LadderConsecutiveFailures = 0
	}
	if record.LadderConsecutiveFailures == ^uint32(0) || record.LastSequence == ^uint64(0) {
		return 0, errors.New("ladder restart counter overflow")
	}
	record.LadderConsecutiveFailures++
	record.LastLadderFailureCode = code
	details, err := json.Marshal(map[string]any{"code": code, "attempt": record.LadderConsecutiveFailures, "output_state": record.OutputState})
	if err != nil {
		return 0, err
	}
	sequence := record.LastSequence + 1
	event := RunnerEventV1{EventID: record.RunnerSessionID + ":" + strconv.FormatUint(sequence, 10), Sequence: sequence, EventType: "session.ladder.restart", EventTime: now.Format(time.RFC3339Nano), State: "active", Details: details}
	if err := ValidateEventV1(event); err != nil {
		return 0, err
	}
	record.LastSequence = sequence
	record.LastEventAt = event.EventTime
	record.PendingEvents = append(record.PendingEvents, event)
	return record.LadderConsecutiveFailures, s.saveLocked(record)
}

// EvaluateOutputHealth emits the one stalled transition for a publisher epoch
// and reports when the durable no-output interval has crossed the fail limit.
func (s *EncryptedFileSessionStoreV1) EvaluateOutputHealth(brokerSessionID string, eventTime time.Time, stallAfter, failAfter time.Duration) (failed bool, stalled bool, err error) {
	if eventTime.IsZero() || stallAfter <= 0 || failAfter <= stallAfter {
		return false, false, errors.New("output health deadlines are invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, _, err := s.loadLocked(brokerSessionID)
	if err != nil {
		return false, false, err
	}
	if record.State != "active" || record.Stopping {
		return false, false, ErrSessionTerminalV1
	}
	if record.IngestOnlineAt == "" {
		return false, false, nil
	}
	base, err := time.Parse(time.RFC3339Nano, record.IngestOnlineAt)
	if err != nil {
		return false, false, errors.New("durable ingest time is invalid")
	}
	if record.LastFinalizedSegmentAt != "" {
		last, parseErr := time.Parse(time.RFC3339Nano, record.LastFinalizedSegmentAt)
		if parseErr != nil {
			return false, false, errors.New("durable output time is invalid")
		}
		if last.After(base) {
			base = last
		}
	}
	elapsed := eventTime.Sub(base)
	if elapsed >= failAfter {
		return true, false, nil
	}
	if elapsed < stallAfter || record.OutputState == OutputStateStalledV1 {
		return false, false, nil
	}
	if err := recordOutputStalledLockedV1(&record, eventTime); err != nil {
		return false, false, err
	}
	if err := s.saveLocked(record); err != nil {
		return false, false, err
	}
	return false, true, nil
}

func (s *EncryptedFileSessionStoreV1) RecordOutputStalled(brokerSessionID string, eventTime time.Time) (bool, error) {
	if eventTime.IsZero() {
		return false, errors.New("output stall time is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, _, err := s.loadLocked(brokerSessionID)
	if err != nil {
		return false, err
	}
	if record.State != "active" || record.Stopping {
		return false, ErrSessionTerminalV1
	}
	if record.OutputState == OutputStateStalledV1 {
		return false, nil
	}
	if err := recordOutputStalledLockedV1(&record, eventTime); err != nil {
		return false, err
	}
	if err := s.saveLocked(record); err != nil {
		return false, err
	}
	return true, nil
}

func recordOutputStalledLockedV1(record *SessionRecordV1, eventTime time.Time) error {
	record.OutputState = OutputStateStalledV1
	record.OutputStateSince = eventTime.UTC().Format(time.RFC3339Nano)
	if record.LastSequence == ^uint64(0) {
		return errors.New("event sequence overflow")
	}
	details, err := outputHealthDetailsV1(*record)
	if err != nil {
		return err
	}
	sequence := record.LastSequence + 1
	event := RunnerEventV1{EventID: record.RunnerSessionID + ":" + strconv.FormatUint(sequence, 10), Sequence: sequence, EventType: "session.output.stalled", EventTime: record.OutputStateSince, State: "active", Details: details}
	if err := ValidateEventV1(event); err != nil {
		return err
	}
	record.LastSequence = sequence
	record.LastEventAt = event.EventTime
	record.PendingEvents = append(record.PendingEvents, event)
	return nil
}

func outputHealthDetailsV1(record SessionRecordV1) (json.RawMessage, error) {
	details := map[string]any{"output_state": record.OutputState, "output_state_since": record.OutputStateSince}
	if record.LastLadderFailureCode != "" {
		details["last_failure_code"] = record.LastLadderFailureCode
	}
	return json.Marshal(details)
}

func (s *EncryptedFileSessionStoreV1) BeginTermination(brokerSessionID, reason string) (SessionRecordV1, bool, error) {
	return s.beginTerminalV1(brokerSessionID, reason, "ended")
}

func (s *EncryptedFileSessionStoreV1) BeginFailure(brokerSessionID, reason string) (SessionRecordV1, bool, error) {
	return s.beginTerminalV1(brokerSessionID, reason, "failed")
}

func (s *EncryptedFileSessionStoreV1) beginTerminalV1(brokerSessionID, reason, terminalState string) (SessionRecordV1, bool, error) {
	if !validCloseReasonV1(reason) {
		return SessionRecordV1{}, false, errors.New("termination reason is invalid")
	}
	if terminalState != "ended" && terminalState != "failed" {
		return SessionRecordV1{}, false, errors.New("terminal target state is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, _, err := s.loadLocked(brokerSessionID)
	if err != nil {
		return SessionRecordV1{}, false, err
	}
	if record.State != "active" {
		return cloneSessionRecordV1(record), false, nil
	}
	if record.Stopping {
		return cloneSessionRecordV1(record), false, nil
	}
	record.Stopping = true
	record.PendingCloseReason = reason
	record.PendingTerminalState = terminalState
	record.PendingKeyActivationID = ""
	if err := s.saveLocked(record); err != nil {
		return SessionRecordV1{}, false, err
	}
	return cloneSessionRecordV1(record), true, nil
}

func (s *EncryptedFileSessionStoreV1) AcknowledgeEvent(brokerSessionID, eventID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, _, err := s.loadLocked(brokerSessionID)
	if err != nil {
		return err
	}
	if len(record.PendingEvents) == 0 || record.PendingEvents[0].EventID != eventID {
		return errors.New("event acknowledgement is out of order")
	}
	record.PendingEvents = append([]RunnerEventV1(nil), record.PendingEvents[1:]...)
	record.CallbackDelivery = nil
	return s.saveLocked(record)
}

// ReserveCallbackAttempt durably records the delivery boundary before the
// callback side effect. If the process exits after the POST but before its
// response is committed, recovery waits for the same bounded retry delay and
// reuses the identical event identity.
func (s *EncryptedFileSessionStoreV1) ReserveCallbackAttempt(brokerSessionID, eventID string, attemptAt time.Time, initialBackoff, maximumBackoff time.Duration) (CallbackDeliveryAttemptV1, error) {
	if eventID == "" || attemptAt.IsZero() || initialBackoff <= 0 || maximumBackoff < initialBackoff {
		return CallbackDeliveryAttemptV1{}, errors.New("callback attempt policy is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, _, err := s.loadLocked(brokerSessionID)
	if err != nil {
		return CallbackDeliveryAttemptV1{}, err
	}
	if len(record.PendingEvents) == 0 || record.PendingEvents[0].EventID != eventID {
		return CallbackDeliveryAttemptV1{}, errors.New("callback attempt is out of order")
	}
	attempts := uint32(1)
	if record.CallbackDelivery != nil {
		if record.CallbackDelivery.EventID != eventID {
			return CallbackDeliveryAttemptV1{}, errors.New("callback attempt cursor is inconsistent")
		}
		if record.CallbackDelivery.Attempts == ^uint32(0) {
			return CallbackDeliveryAttemptV1{}, errors.New("callback attempt count overflow")
		}
		nextAttempt, err := time.Parse(time.RFC3339Nano, record.CallbackDelivery.NextAttemptAt)
		if err != nil {
			return CallbackDeliveryAttemptV1{}, errors.New("callback retry time is invalid")
		}
		if attemptAt.Before(nextAttempt) {
			return CallbackDeliveryAttemptV1{}, ErrCallbackNotDueV1
		}
		attempts = record.CallbackDelivery.Attempts + 1
	}
	delay := callbackRetryDelayV1(eventID, attempts, initialBackoff, maximumBackoff)
	attempt := CallbackDeliveryAttemptV1{
		EventID: eventID, Attempts: attempts,
		LastAttemptAt: attemptAt.UTC().Format(time.RFC3339Nano),
		NextAttemptAt: attemptAt.Add(delay).UTC().Format(time.RFC3339Nano),
	}
	record.CallbackDelivery = &attempt
	if err := s.saveLocked(record); err != nil {
		return CallbackDeliveryAttemptV1{}, err
	}
	return attempt, nil
}

func (s *EncryptedFileSessionStoreV1) RecordCallbackRetryFailure(brokerSessionID, eventID string, statusCode int, errorCode string) error {
	if !validCallbackFailureV1(statusCode, errorCode, true) {
		return errors.New("callback retry failure is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, _, err := s.loadLocked(brokerSessionID)
	if err != nil {
		return err
	}
	if record.CallbackDelivery == nil || record.CallbackDelivery.EventID != eventID || len(record.PendingEvents) == 0 || record.PendingEvents[0].EventID != eventID {
		return errors.New("callback retry cursor is inconsistent")
	}
	record.CallbackDelivery.LastStatusCode = statusCode
	record.CallbackDelivery.LastErrorCode = errorCode
	return s.saveLocked(record)
}

// ParkCallbackRejection atomically removes a permanently rejected head event
// and retains a bounded, credential-free audit copy. It deliberately does not
// enqueue another callback event onto the rejected protocol path.
func (s *EncryptedFileSessionStoreV1) ParkCallbackRejection(brokerSessionID, eventID string, statusCode int, errorCode string, rejectedAt time.Time) error {
	if rejectedAt.IsZero() || !validCallbackFailureV1(statusCode, errorCode, false) {
		return errors.New("callback rejection is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, _, err := s.loadLocked(brokerSessionID)
	if err != nil {
		return err
	}
	if len(record.PendingEvents) == 0 || record.PendingEvents[0].EventID != eventID || record.CallbackDelivery == nil || record.CallbackDelivery.EventID != eventID {
		return errors.New("callback rejection is out of order")
	}
	letter := CallbackDeadLetterV1{
		Event: record.PendingEvents[0], Attempts: record.CallbackDelivery.Attempts,
		StatusCode: statusCode, ErrorCode: errorCode,
		RejectedAt: rejectedAt.UTC().Format(time.RFC3339Nano),
	}
	record.PendingEvents = append([]RunnerEventV1(nil), record.PendingEvents[1:]...)
	record.CallbackDelivery = nil
	if record.CallbackRejectedTotal == ^uint64(0) {
		return errors.New("callback rejection count overflow")
	}
	record.CallbackRejectedTotal++
	record.CallbackDeadLetters = append(record.CallbackDeadLetters, letter)
	if len(record.CallbackDeadLetters) > maxCallbackDeadLettersV1 {
		record.CallbackDeadLetters = append([]CallbackDeadLetterV1(nil), record.CallbackDeadLetters[len(record.CallbackDeadLetters)-maxCallbackDeadLettersV1:]...)
	}
	return s.saveLocked(record)
}

func (s *EncryptedFileSessionStoreV1) ClearTerminalSecrets(brokerSessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, _, err := s.loadLocked(brokerSessionID)
	if err != nil {
		return err
	}
	if record.State == "active" || len(record.PendingEvents) != 0 {
		return errors.New("terminal secrets cannot be cleared before final event acknowledgement")
	}
	record.WrappedKeyNonce = ""
	record.WrappedKeyCiphertext = ""
	record.SecretNonce = ""
	record.SecretCiphertext = ""
	return s.saveLocked(record)
}

func (s *EncryptedFileSessionStoreV1) Recoverable() ([]SessionRecordV1, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var records []SessionRecordV1
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := stringsTrimSuffixV1(entry.Name(), ".json")
		record, _, err := s.loadLocked(id)
		if err != nil {
			return nil, err
		}
		if record.State == "active" || len(record.PendingEvents) != 0 || record.WrappedKeyCiphertext != "" {
			records = append(records, cloneSessionRecordV1(record))
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].BrokerSessionID < records[j].BrokerSessionID })
	return records, nil
}

func (s *EncryptedFileSessionStoreV1) LoadByRunnerSessionID(runnerSessionID string) (SessionRecordV1, *SessionSecretsV1, error) {
	if !opaqueIDPattern.MatchString(runnerSessionID) {
		return SessionRecordV1{}, nil, errors.New("invalid runner session ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return SessionRecordV1{}, nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := stringsTrimSuffixV1(entry.Name(), ".json")
		record, secrets, err := s.loadLocked(id)
		if err != nil {
			return SessionRecordV1{}, nil, err
		}
		if record.RunnerSessionID == runnerSessionID {
			copy := cloneSessionRecordV1(record)
			if secrets == nil {
				return copy, nil, nil
			}
			secretCopy := cloneSessionSecretsV1(*secrets)
			return copy, &secretCopy, nil
		}
	}
	return SessionRecordV1{}, nil, os.ErrNotExist
}

func (s *EncryptedFileSessionStoreV1) loadLocked(id string) (SessionRecordV1, *SessionSecretsV1, error) {
	if !opaqueIDPattern.MatchString(id) {
		return SessionRecordV1{}, nil, errors.New("invalid broker session ID")
	}
	body, err := os.ReadFile(s.path(id))
	if err != nil {
		return SessionRecordV1{}, nil, err
	}
	var record SessionRecordV1
	if err := decodeStrictBytesV1(body, &record); err != nil {
		return SessionRecordV1{}, nil, fmt.Errorf("decode session record: %w", err)
	}
	if err := s.verifyIntegrityV1(record); err != nil {
		return SessionRecordV1{}, nil, err
	}
	// Records created before output-health fields were added remain readable;
	// the next mutation seals the explicit waiting state into the record.
	if record.OutputState == "" {
		record.OutputState = OutputStateWaitingV1
		record.OutputStateSince = record.LastEventAt
		if record.OutputStateSince == "" {
			record.OutputStateSince = time.Unix(0, 0).UTC().Format(time.RFC3339Nano)
		}
	}
	if record.Stopping && record.PendingTerminalState == "" {
		record.PendingTerminalState = "ended"
	}
	if err := validateSessionRecordV1(record, id); err != nil {
		return SessionRecordV1{}, nil, err
	}
	if record.WrappedKeyCiphertext == "" {
		return record, nil, nil
	}
	wrappedKeyNonce, err := hex.DecodeString(record.WrappedKeyNonce)
	if err != nil || len(wrappedKeyNonce) != s.aead.NonceSize() {
		return SessionRecordV1{}, nil, errors.New("session wrapped-key nonce is invalid")
	}
	wrappedKeyCiphertext, err := hex.DecodeString(record.WrappedKeyCiphertext)
	if err != nil {
		return SessionRecordV1{}, nil, errors.New("session wrapped-key ciphertext is invalid")
	}
	dataKey, err := s.aead.Open(nil, wrappedKeyNonce, wrappedKeyCiphertext, []byte(id+":dek"))
	if err != nil || len(dataKey) != 32 {
		return SessionRecordV1{}, nil, errors.New("session data-key decryption failed")
	}
	secretAEAD, err := newAESGCMV1(dataKey)
	if err != nil {
		return SessionRecordV1{}, nil, errors.New("session data key is invalid")
	}
	nonce, err := hex.DecodeString(record.SecretNonce)
	if err != nil || len(nonce) != secretAEAD.NonceSize() {
		return SessionRecordV1{}, nil, errors.New("session secret nonce is invalid")
	}
	ciphertext, err := hex.DecodeString(record.SecretCiphertext)
	if err != nil {
		return SessionRecordV1{}, nil, errors.New("session secret ciphertext is invalid")
	}
	plaintext, err := secretAEAD.Open(nil, nonce, ciphertext, []byte(id+":secrets"))
	if err != nil {
		return SessionRecordV1{}, nil, errors.New("session secret decryption failed")
	}
	var secrets SessionSecretsV1
	if err := decodeStrictBytesV1(plaintext, &secrets); err != nil {
		return SessionRecordV1{}, nil, errors.New("session secret payload is invalid")
	}
	if secrets.CreateRequest.SessionID != id || secrets.CreateResponse.RunnerSessionID != record.RunnerSessionID {
		return SessionRecordV1{}, nil, errors.New("session secret identity mismatch")
	}
	if err := validateSessionSecretsV1(record, secrets); err != nil {
		return SessionRecordV1{}, nil, err
	}
	return record, &secrets, nil
}

func (s *EncryptedFileSessionStoreV1) sealLocked(record *SessionRecordV1, secrets SessionSecretsV1) error {
	body, err := json.Marshal(secrets)
	if err != nil {
		return err
	}
	dataKey := make([]byte, 32)
	if _, err := rand.Read(dataKey); err != nil {
		return err
	}
	secretAEAD, err := newAESGCMV1(dataKey)
	if err != nil {
		return err
	}
	wrappedKeyNonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(wrappedKeyNonce); err != nil {
		return err
	}
	wrappedKeyCiphertext := s.aead.Seal(nil, wrappedKeyNonce, dataKey, []byte(record.BrokerSessionID+":dek"))
	nonce := make([]byte, secretAEAD.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ciphertext := secretAEAD.Seal(nil, nonce, body, []byte(record.BrokerSessionID+":secrets"))
	record.WrappedKeyNonce = hex.EncodeToString(wrappedKeyNonce)
	record.WrappedKeyCiphertext = hex.EncodeToString(wrappedKeyCiphertext)
	record.SecretNonce = hex.EncodeToString(nonce)
	record.SecretCiphertext = hex.EncodeToString(ciphertext)
	return nil
}

func (s *EncryptedFileSessionStoreV1) saveLocked(record SessionRecordV1) error {
	integrity, err := s.integrityV1(record)
	if err != nil {
		return err
	}
	record.IntegritySHA256 = integrity
	temporary, err := os.CreateTemp(s.dir, ".session-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if err := json.NewEncoder(temporary).Encode(record); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.path(record.BrokerSessionID)); err != nil {
		return err
	}
	directory, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (s *EncryptedFileSessionStoreV1) path(id string) string { return filepath.Join(s.dir, id+".json") }

func (s *EncryptedFileSessionStoreV1) integrityV1(record SessionRecordV1) (string, error) {
	record.IntegritySHA256 = ""
	body, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func (s *EncryptedFileSessionStoreV1) verifyIntegrityV1(record SessionRecordV1) error {
	if !workIDPattern.MatchString(record.IntegritySHA256) {
		return errors.New("session record integrity is invalid")
	}
	expected, err := s.integrityV1(record)
	if err != nil || !hmac.Equal([]byte(expected), []byte(record.IntegritySHA256)) {
		return errors.New("session record integrity check failed")
	}
	return nil
}

func validateSessionRecordV1(record SessionRecordV1, id string) error {
	if record.Version != sessionRecordVersionV1 || record.BrokerSessionID != id || !opaqueIDPattern.MatchString(record.RunnerSessionID) || !workIDPattern.MatchString(record.CreateFingerprint) || !validStateV1(record.State) {
		return errors.New("session record identity is invalid")
	}
	secretFields := []string{record.WrappedKeyNonce, record.WrappedKeyCiphertext, record.SecretNonce, record.SecretCiphertext}
	hasSecrets := secretFields[0] != ""
	for _, field := range secretFields[1:] {
		if (field != "") != hasSecrets {
			return errors.New("session record secret envelope is incomplete")
		}
	}
	if record.State == "active" && !hasSecrets {
		return errors.New("session record secret envelope is incomplete")
	}
	if !validOutputStateV1(record.OutputState) || !validLadderFailureCodeV1(record.LastLadderFailureCode, true) {
		return errors.New("session record output health is invalid")
	}
	for _, stamp := range []string{record.OutputStateSince, record.IngestOnlineAt, record.FirstFinalizedSegmentAt, record.LastFinalizedSegmentAt, record.LadderFailureWindowAt} {
		if stamp != "" {
			if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
				return errors.New("session record output health time is invalid")
			}
		}
	}
	if record.OutputStateSince == "" || (record.LadderConsecutiveFailures > 0) != (record.LadderFailureWindowAt != "") {
		return errors.New("session record output health cursor is invalid")
	}
	if !workIDPattern.MatchString(record.GrantAudit.SecretSHA256) || !opaqueIDPattern.MatchString(record.GrantAudit.ID) || len(record.GrantAudit.Operations) != 1 || record.GrantAudit.Operations[0] != GrantOperationV1 {
		return errors.New("session record grant audit is invalid")
	}
	if _, err := time.Parse(time.RFC3339, record.GrantAudit.ExpiresAt); err != nil {
		return errors.New("session record grant expiry is invalid")
	}
	if err := validateURLScheme(record.RuntimePublic.RTMPURL, "rtmp", "rtmps"); err != nil {
		return errors.New("session record RTMP coordinate is invalid")
	}
	if err := validateHTTPURL(record.RuntimePublic.HLSURL); err != nil {
		return errors.New("session record HLS coordinate is invalid")
	}
	if err := validateHTTPURL(record.RuntimePublic.KeyIssueURL); err != nil {
		return errors.New("session record key issuance coordinate is invalid")
	}
	terminal := record.State == "ended" || record.State == "failed"
	if terminal != (record.CloseReason != "") || (record.CloseReason != "" && !validCloseReasonV1(record.CloseReason)) {
		return errors.New("session record terminal state is invalid")
	}
	if record.Stopping != (record.PendingCloseReason != "") || record.Stopping != (record.PendingTerminalState != "") || (record.PendingTerminalState != "" && record.PendingTerminalState != "ended" && record.PendingTerminalState != "failed") || (record.PendingCloseReason != "" && !validCloseReasonV1(record.PendingCloseReason)) || terminal && record.Stopping {
		return errors.New("session record stopping state is invalid")
	}
	if record.PendingKeyActivationID != "" && (!opaqueIDPattern.MatchString(record.PendingKeyActivationID) || record.State != "active" || record.Stopping) {
		return errors.New("session record key activation is invalid")
	}
	var previousSequence uint64
	var previousUsage uint64
	var hasUsage bool
	for _, event := range record.PendingEvents {
		if err := ValidateEventV1(event); err != nil || event.EventID != record.RunnerSessionID+":"+strconv.FormatUint(event.Sequence, 10) || event.Sequence <= previousSequence || event.Sequence > record.LastSequence {
			return errors.New("session record event outbox is invalid")
		}
		if event.Usage != nil && hasUsage && event.Usage.Total < previousUsage {
			return errors.New("session record event outbox usage is invalid")
		}
		previousSequence = event.Sequence
		if event.Usage != nil {
			previousUsage = event.Usage.Total
			hasUsage = true
		}
	}
	if record.CallbackDelivery != nil {
		attempt := record.CallbackDelivery
		if len(record.PendingEvents) == 0 || record.PendingEvents[0].EventID != attempt.EventID || attempt.Attempts == 0 || !validCallbackFailureV1(attempt.LastStatusCode, attempt.LastErrorCode, true) {
			return errors.New("session record callback delivery cursor is invalid")
		}
		lastAttempt, lastErr := time.Parse(time.RFC3339Nano, attempt.LastAttemptAt)
		nextAttempt, nextErr := time.Parse(time.RFC3339Nano, attempt.NextAttemptAt)
		if lastErr != nil || nextErr != nil || nextAttempt.Before(lastAttempt) {
			return errors.New("session record callback delivery time is invalid")
		}
	}
	if len(record.CallbackDeadLetters) > maxCallbackDeadLettersV1 {
		return errors.New("session record callback dead letters exceed limit")
	}
	if uint64(len(record.CallbackDeadLetters)) > record.CallbackRejectedTotal {
		return errors.New("session record callback rejection cursor is invalid")
	}
	for _, letter := range record.CallbackDeadLetters {
		if ValidateEventV1(letter.Event) != nil || letter.Event.EventID != record.RunnerSessionID+":"+strconv.FormatUint(letter.Event.Sequence, 10) || letter.Event.Sequence > record.LastSequence || letter.Attempts == 0 || !validCallbackFailureV1(letter.StatusCode, letter.ErrorCode, false) {
			return errors.New("session record callback dead letter is invalid")
		}
		if _, err := time.Parse(time.RFC3339Nano, letter.RejectedAt); err != nil {
			return errors.New("session record callback dead-letter time is invalid")
		}
	}
	if record.UsageTotal > 0 && record.LastSequence == 0 {
		return errors.New("session record usage cursor is invalid")
	}
	hasMeteringCursor := record.MeteredMicroseconds > 0 || len(record.MeteredSegmentSHA256) > 0
	if (hasMeteringCursor && record.MeteredMicroseconds/1_000_000 != record.UsageTotal) || (len(record.MeteredSegmentSHA256) > 0 && record.MeteredMicroseconds == 0) {
		return errors.New("session record metering cursor is invalid")
	}
	seenSegments := make(map[string]struct{}, len(record.MeteredSegmentSHA256))
	for _, identity := range record.MeteredSegmentSHA256 {
		if !workIDPattern.MatchString(identity) {
			return errors.New("session record segment identity is invalid")
		}
		if _, exists := seenSegments[identity]; exists {
			return errors.New("session record segment identity is duplicated")
		}
		seenSegments[identity] = struct{}{}
	}
	if record.LastEventAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, record.LastEventAt); err != nil || record.LastSequence == 0 {
			return errors.New("session record event time is invalid")
		}
	}
	if len(record.PendingEvents) > 0 {
		last := record.PendingEvents[len(record.PendingEvents)-1]
		if last.Sequence != record.LastSequence || (last.Usage != nil && last.Usage.Total != record.UsageTotal) {
			return errors.New("session record event cursor is inconsistent")
		}
	}
	return nil
}

func validateSessionSecretsV1(record SessionRecordV1, secrets SessionSecretsV1) error {
	if err := ValidateCreateRequestV1(secrets.CreateRequest); err != nil {
		return errors.New("session secret create request is invalid")
	}
	if err := ValidateCreateResponseV1(secrets.CreateResponse); err != nil {
		return errors.New("session secret create response is invalid")
	}
	fingerprint, err := CreateFingerprintV1(secrets.CreateRequest)
	if err != nil || fingerprint != record.CreateFingerprint || secrets.CreateRequest.SessionID != record.BrokerSessionID || secrets.CreateResponse.RunnerSessionID != record.RunnerSessionID || !reflect.DeepEqual(secrets.CreateResponse.Runtime.Public, record.RuntimePublic) {
		return errors.New("session secret create identity is invalid")
	}
	grant := secrets.CreateResponse.Runtime.Grants[0]
	if grant.ID != record.GrantAudit.ID || !reflect.DeepEqual(grant.Operations, record.GrantAudit.Operations) || grant.ExpiresAt != record.GrantAudit.ExpiresAt || secretSHA256V1(grant.Secret) != record.GrantAudit.SecretSHA256 {
		return errors.New("session secret grant identity is invalid")
	}
	if secrets.CurrentKeyID != "" {
		if _, ok := secrets.KeyIssues[secrets.CurrentKeyID]; !ok {
			return errors.New("session current stream key is missing")
		}
	}
	if record.PendingKeyActivationID != "" && secrets.CurrentKeyID != record.PendingKeyActivationID {
		return errors.New("session pending key activation is invalid")
	}
	for id, issue := range secrets.KeyIssues {
		fingerprint, err := KeyIssueFingerprintV1(issue.Request)
		if err != nil || id != issue.Request.RequestID || fingerprint != issue.Fingerprint {
			return errors.New("session key issue identity is invalid")
		}
		if id == secrets.CurrentKeyID {
			if err := ValidateStreamKeyIssueResponseV1(issue.Request, issue.Response); err != nil {
				return errors.New("session current key response is invalid")
			}
		} else if issue.Response.RequestID != id || issue.Response.StreamKey != "" {
			return errors.New("session superseded key response is invalid")
		}
	}
	return nil
}

func newAESGCMV1(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func deriveKeyV1(master []byte, label string) []byte {
	mac := hmac.New(sha256.New, master)
	_, _ = mac.Write([]byte(label))
	return mac.Sum(nil)
}

func decodeStrictBytesV1(body []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}

func secretSHA256V1(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func cloneSessionRecordV1(value SessionRecordV1) SessionRecordV1 {
	body, _ := json.Marshal(value)
	var clone SessionRecordV1
	_ = json.Unmarshal(body, &clone)
	return clone
}

func cloneSessionSecretsV1(value SessionSecretsV1) SessionSecretsV1 {
	body, _ := json.Marshal(value)
	var clone SessionSecretsV1
	_ = json.Unmarshal(body, &clone)
	return clone
}

func stringsTrimSuffixV1(value, suffix string) string {
	if len(value) >= len(suffix) && value[len(value)-len(suffix):] == suffix {
		return value[:len(value)-len(suffix)]
	}
	return value
}
