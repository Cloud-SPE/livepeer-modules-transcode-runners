package liverunner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	PaidSessionProtocolV1  = "paid-session/v1"
	RuntimeSchemaV1        = "rtmp-hls/v1"
	SessionParamsSchemaV1  = "rtmp-hls-session/v1"
	PaidSessionVersionV1   = "1.2.0"
	RuntimeSchemaVersionV1 = "1.1.0"
	SessionParamsVersionV1 = "1.0.0"
	WorkUnitV1             = "output_seconds"
	GrantOperationV1       = "stream-key-issue"
	OutputStateWaitingV1   = "waiting"
	OutputStateProducingV1 = "producing"
	OutputStateStalledV1   = "stalled"
)

var (
	opaqueIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	workIDPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	closeReasonsV1  = map[string]struct{}{
		"gateway_close": {}, "lease_expired": {}, "heartbeat_lost": {},
		"runway_exhausted": {}, "refill_refused": {}, "recovery_failed": {},
		"payment_unrecoverable": {}, "runner_failed": {}, "ingest_failed": {},
		"output_failed": {},
		// Broker lifecycle codes and historical gateway reasons may already be
		// durably pinned in winding-down sessions. Accept these finite codes
		// so retries can finish without changing the recorded close reason.
		"runner_ended": {}, "insufficient_balance": {}, "authorization_exhausted": {},
		"open_failed": {}, "capacity_exhausted": {},
		"lease_exhausted": {}, "customer_end": {}, "publisher_disconnect": {},
		"broker_ended": {}, "relay_failure": {}, "refill_preannounced_refusal": {},
		"refill_policy_exhausted": {}, "refill_total_exceeded": {},
	}
	ladderFailureCodesV1 = map[string]struct{}{
		"encoder_init_failed": {}, "hwaccel_init_failed": {}, "input_unavailable": {},
		"output_rejected": {}, "process_killed": {}, "unknown": {},
	}
)

type RunnerCreateRequestV1 struct {
	SessionID     string                 `json:"session_id"`
	WorkID        string                 `json:"work_id"`
	Capability    string                 `json:"capability"`
	Offering      string                 `json:"offering"`
	SessionParams RTMPHLSSessionParamsV1 `json:"session_params"`
	CallbackURL   string                 `json:"callback_url"`
	CallbackToken string                 `json:"callback_token"`
}

type RTMPHLSSessionParamsV1 struct {
	Schema            string    `json:"schema"`
	PublisherMode     string    `json:"publisher_mode"`
	OutputProfile     string    `json:"output_profile"`
	MeteringRendition string    `json:"metering_rendition"`
	Storage           StorageV1 `json:"storage"`
}

type StorageV1 struct {
	Kind string              `json:"kind"`
	S3   *S3SessionStorageV1 `json:"s3,omitempty"`
}

type S3SessionStorageV1 struct {
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	Prefix          string `json:"prefix"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
	ForcePathStyle  bool   `json:"force_path_style,omitempty"`
}

type RunnerCreateResponseV1 struct {
	RunnerSessionID string              `json:"runner_session_id"`
	Runtime         RuntimeDescriptorV1 `json:"runtime"`
}

type RuntimeDescriptorV1 struct {
	Schema string          `json:"schema"`
	Public RuntimePublicV1 `json:"public"`
	Grants []GrantV1       `json:"grants"`
}

type RuntimePublicV1 struct {
	RTMPURL     string `json:"rtmp_url"`
	HLSURL      string `json:"hls_url"`
	KeyIssueURL string `json:"key_issue_url"`
	StatusURL   string `json:"status_url,omitempty"`
}

type GrantV1 struct {
	ID         string   `json:"id"`
	Operations []string `json:"operations"`
	Secret     string   `json:"secret"`
	ExpiresAt  string   `json:"expires_at"`
}

type RunnerStatusV1 struct {
	RunnerSessionID       string                     `json:"runner_session_id"`
	State                 string                     `json:"state"`
	Usage                 UsageV1                    `json:"usage"`
	LastSequence          uint64                     `json:"last_sequence"`
	CloseReason           string                     `json:"close_reason,omitempty"`
	OutputState           string                     `json:"output_state"`
	LastFailureCode       string                     `json:"last_failure_code,omitempty"`
	CallbackRejectedTotal uint64                     `json:"callback_rejected_total,omitempty"`
	LastCallbackRejection *CallbackRejectionStatusV1 `json:"last_callback_rejection,omitempty"`
}

type CallbackRejectionStatusV1 struct {
	EventType  string `json:"event_type"`
	StatusCode int    `json:"status_code"`
	ErrorCode  string `json:"error_code"`
	RejectedAt string `json:"rejected_at"`
}

type UsageV1 struct {
	Unit  string `json:"unit"`
	Total uint64 `json:"total"`
}

type RunnerEventV1 struct {
	EventID     string          `json:"event_id"`
	Sequence    uint64          `json:"sequence"`
	EventType   string          `json:"event_type"`
	EventTime   string          `json:"event_time"`
	State       string          `json:"state"`
	Usage       *UsageV1        `json:"usage,omitempty"`
	CloseReason *string         `json:"close_reason"`
	Details     json.RawMessage `json:"details"`
}

type EventCursorV1 struct {
	Sequence   uint64
	UsageTotal uint64
	HasUsage   bool
	SeenIDs    map[string]struct{}
}

type StreamKeyIssueRequestV1 struct {
	RequestID string `json:"request_id"`
	Audience  string `json:"audience"`
}

type StreamKeyIssueResponseV1 struct {
	RequestID string `json:"request_id"`
	StreamKey string `json:"stream_key"`
	ExpiresAt string `json:"expires_at"`
}

type ReplayDispositionV1 string

const (
	ReplayIdenticalV1 ReplayDispositionV1 = "replay_identical"
	RejectIDReuseV1   ReplayDispositionV1 = "reject_id_reuse"
)

type TerminateRequestV1 struct {
	Reason string `json:"reason"`
}
type TerminateResponseV1 struct {
	RunnerSessionID string `json:"runner_session_id"`
	State           string `json:"state"`
	CloseReason     string `json:"close_reason"`
}

type LiveRunnerContractDocumentV1 struct {
	CapabilityID        string            `json:"capability_id"`
	Protocol            string            `json:"protocol"`
	DescriptorSchemas   []string          `json:"descriptor_schemas"`
	WorkUnit            RunnerWorkUnitV1  `json:"work_unit"`
	Metering            string            `json:"metering"`
	Heartbeat           HeartbeatV1       `json:"heartbeat"`
	Readiness           RunnerReadinessV1 `json:"readiness"`
	SessionParamsSchema json.RawMessage   `json:"session_params_schema"`
	Paths               RunnerPathsV1     `json:"paths"`
	Identity            map[string]string `json:"identity"`
	SchemaVersions      map[string]string `json:"schema_versions"`
}

type RunnerWorkUnitV1 struct {
	Name string `json:"name"`
}

type HeartbeatV1 struct {
	IntervalSeconds uint32 `json:"interval_seconds"`
}
type RunnerReadinessV1 struct {
	Type string `json:"type"`
	Path string `json:"path"`
}
type RunnerPathsV1 struct {
	Create    string `json:"create"`
	Status    string `json:"status"`
	Terminate string `json:"terminate"`
}

func ValidateCreateRequestV1(value RunnerCreateRequestV1) error {
	if !opaqueIDPattern.MatchString(value.SessionID) || !validAuthorizationIDV1(value.WorkID) {
		return errors.New("session or work identity is invalid")
	}
	if strings.TrimSpace(value.Capability) == "" || strings.TrimSpace(value.Offering) == "" {
		return errors.New("capability and offering are required")
	}
	if value.SessionParams.Schema != SessionParamsSchemaV1 || (value.SessionParams.PublisherMode != "gateway-relay" && value.SessionParams.PublisherMode != "direct-publisher") {
		return errors.New("session parameter schema or publisher mode is invalid")
	}
	if !opaqueIDPattern.MatchString(value.SessionParams.OutputProfile) || !opaqueIDPattern.MatchString(value.SessionParams.MeteringRendition) {
		return errors.New("output profile or metering rendition is invalid")
	}
	if err := validateStorageV1(value.SessionParams.Storage); err != nil {
		return err
	}
	if err := validateHTTPURL(value.CallbackURL); err != nil || value.CallbackToken == "" {
		return errors.New("callback coordinates are invalid")
	}
	return nil
}

// A paid-session/v1 work_id mirrors SpendAuthorization.authorization_id. It
// is bounded opaque correlation state, not a request-body or integrity hash.
// Keep this semantic check separate from workIDPattern, which is deliberately
// restricted to lowercase SHA-256 hex used elsewhere in the runner.
func validAuthorizationIDV1(value string) bool {
	return opaqueIDPattern.MatchString(value)
}

func ValidateCreateResponseV1(value RunnerCreateResponseV1) error {
	if !opaqueIDPattern.MatchString(value.RunnerSessionID) || value.Runtime.Schema != RuntimeSchemaV1 {
		return errors.New("runner session or runtime schema is invalid")
	}
	if err := validateURLScheme(value.Runtime.Public.RTMPURL, "rtmp", "rtmps"); err != nil {
		return fmt.Errorf("rtmp_url: %w", err)
	}
	for name, raw := range map[string]string{"hls_url": value.Runtime.Public.HLSURL, "key_issue_url": value.Runtime.Public.KeyIssueURL} {
		if err := validateHTTPURL(raw); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if value.Runtime.Public.StatusURL != "" {
		if err := validateHTTPURL(value.Runtime.Public.StatusURL); err != nil {
			return err
		}
	}
	if len(value.Runtime.Grants) != 1 {
		return errors.New("rtmp-hls requires exactly one grant")
	}
	grant := value.Runtime.Grants[0]
	if !opaqueIDPattern.MatchString(grant.ID) || len(grant.Operations) != 1 || grant.Operations[0] != GrantOperationV1 || grant.Secret == "" {
		return errors.New("stream-key-issue grant is invalid")
	}
	if _, err := time.Parse(time.RFC3339, grant.ExpiresAt); err != nil {
		return errors.New("grant expiry is invalid")
	}
	return nil
}

func ValidateStatusV1(value RunnerStatusV1) error {
	if !opaqueIDPattern.MatchString(value.RunnerSessionID) || !validStateV1(value.State) || value.Usage.Unit != WorkUnitV1 || !validOutputStateV1(value.OutputState) || !validLadderFailureCodeV1(value.LastFailureCode, true) {
		return errors.New("runner status is invalid")
	}
	if (value.State == "ended" || value.State == "failed") != (value.CloseReason != "") {
		return errors.New("terminal status requires a close reason")
	}
	if value.CloseReason != "" && !validCloseReasonV1(value.CloseReason) {
		return errors.New("terminal status close reason is invalid")
	}
	if (value.CallbackRejectedTotal == 0) != (value.LastCallbackRejection == nil) {
		return errors.New("runner callback rejection status is invalid")
	}
	if rejection := value.LastCallbackRejection; rejection != nil {
		if rejection.EventType == "" || !validCallbackFailureV1(rejection.StatusCode, rejection.ErrorCode, false) {
			return errors.New("runner callback rejection status is invalid")
		}
		if _, err := time.Parse(time.RFC3339Nano, rejection.RejectedAt); err != nil {
			return errors.New("runner callback rejection time is invalid")
		}
	}
	return nil
}

func ValidateEventV1(value RunnerEventV1) error {
	if !opaqueIDPattern.MatchString(value.EventID) || value.Sequence == 0 {
		return errors.New("event identity is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, value.EventTime); err != nil {
		return errors.New("event time is invalid")
	}
	if !validStateV1(value.State) {
		return errors.New("event state is invalid")
	}
	switch value.EventType {
	case "session.started", "session.heartbeat", "session.ladder.restart", "session.output.stalled":
		if value.State != "active" {
			return errors.New("liveness event must be active")
		}
	case "session.usage.tick":
		if value.State != "active" || value.Usage == nil {
			return errors.New("usage event is invalid")
		}
	case "session.failed":
		if value.State != "failed" || value.Usage == nil || value.CloseReason == nil || !validCloseReasonV1(*value.CloseReason) {
			return errors.New("failed event is invalid")
		}
	case "session.ended":
		if value.State != "ended" || value.Usage == nil || value.CloseReason == nil || !validCloseReasonV1(*value.CloseReason) {
			return errors.New("ended event is invalid")
		}
	default:
		return errors.New("event type is invalid")
	}
	if value.Usage != nil && value.Usage.Unit != WorkUnitV1 {
		return errors.New("event work unit is invalid")
	}
	if err := validateSafeEventDetailsV1(value.Details); err != nil {
		return err
	}
	return nil
}

func validateSafeEventDetailsV1(raw json.RawMessage) error {
	if len(raw) == 0 || len(raw) > 2048 || !json.Valid(raw) {
		return errors.New("event details must be bounded JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var details map[string]any
	if err := decoder.Decode(&details); err != nil || details == nil {
		return errors.New("event details must be a JSON object")
	}
	if unsafeEventDetailV1(details) {
		return errors.New("event details contain credential-bearing data")
	}
	return nil
}

func unsafeEventDetailV1(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			lower := strings.ToLower(key)
			for _, forbidden := range []string{"token", "secret", "credential", "authorization", "stream_key", "streamkey", "url", "password"} {
				if strings.Contains(lower, forbidden) {
					return true
				}
			}
			if unsafeEventDetailV1(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if unsafeEventDetailV1(child) {
				return true
			}
		}
	case string:
		lower := strings.ToLower(typed)
		return strings.Contains(lower, "://") || strings.Contains(lower, "sig=") || strings.Contains(lower, "bearer ")
	}
	return false
}

func ValidateEventAdvanceV1(previous, next RunnerEventV1) error {
	if err := ValidateEventV1(next); err != nil {
		return err
	}
	if next.Sequence <= previous.Sequence || next.EventID == previous.EventID {
		return errors.New("event sequence did not advance")
	}
	if previous.Usage != nil && next.Usage != nil && next.Usage.Total < previous.Usage.Total {
		return errors.New("cumulative usage decreased")
	}
	return nil
}

func (cursor *EventCursorV1) Accept(event RunnerEventV1) error {
	if err := ValidateEventV1(event); err != nil {
		return err
	}
	if event.Sequence <= cursor.Sequence {
		return errors.New("event sequence did not advance")
	}
	if cursor.SeenIDs == nil {
		cursor.SeenIDs = make(map[string]struct{})
	}
	if _, duplicate := cursor.SeenIDs[event.EventID]; duplicate {
		return errors.New("event ID was reused")
	}
	if event.Usage != nil && cursor.HasUsage && event.Usage.Total < cursor.UsageTotal {
		return errors.New("cumulative usage decreased")
	}
	cursor.Sequence = event.Sequence
	cursor.SeenIDs[event.EventID] = struct{}{}
	if event.Usage != nil {
		cursor.UsageTotal = event.Usage.Total
		cursor.HasUsage = true
	}
	return nil
}

func ValidateStreamKeyIssueRequestV1(value StreamKeyIssueRequestV1) error {
	if !opaqueIDPattern.MatchString(value.RequestID) || (value.Audience != "gateway-relay" && value.Audience != "direct-publisher") {
		return errors.New("key issuance request is invalid")
	}
	return nil
}

func ValidateStreamKeyIssueResponseV1(request StreamKeyIssueRequestV1, response StreamKeyIssueResponseV1) error {
	if response.RequestID != request.RequestID || strings.TrimSpace(response.StreamKey) == "" || len(response.StreamKey) > 512 {
		return errors.New("key issuance response is invalid")
	}
	if _, err := time.Parse(time.RFC3339, response.ExpiresAt); err != nil {
		return errors.New("key expiry is invalid")
	}
	return nil
}

func ClassifyReplayV1(recordedFingerprint, incomingFingerprint string) ReplayDispositionV1 {
	if recordedFingerprint == incomingFingerprint {
		return ReplayIdenticalV1
	}
	return RejectIDReuseV1
}

func ValidateTerminateV1(request TerminateRequestV1, response TerminateResponseV1) error {
	if !validCloseReasonV1(request.Reason) || !opaqueIDPattern.MatchString(response.RunnerSessionID) || (response.State != "ended" && response.State != "failed") || !validCloseReasonV1(response.CloseReason) {
		return errors.New("termination contract is invalid")
	}
	return nil
}

func LiveRunnerContractV1() LiveRunnerContractDocumentV1 {
	return LiveRunnerContractDocumentV1{
		CapabilityID: "video:transcode.live", Protocol: PaidSessionProtocolV1,
		DescriptorSchemas: []string{RuntimeSchemaV1}, Metering: "runner-reported",
		WorkUnit: RunnerWorkUnitV1{Name: WorkUnitV1}, Heartbeat: HeartbeatV1{IntervalSeconds: 5},
		Readiness:           RunnerReadinessV1{Type: "http-status", Path: "/ready"},
		Paths:               RunnerPathsV1{Create: "/v1/sessions", Status: "/v1/sessions/{id}", Terminate: "/v1/sessions/{id}"},
		Identity:            map[string]string{"provider": "livepeer-live-runner"},
		SchemaVersions:      map[string]string{PaidSessionProtocolV1: PaidSessionVersionV1, RuntimeSchemaV1: RuntimeSchemaVersionV1, SessionParamsSchemaV1: SessionParamsVersionV1},
		SessionParamsSchema: json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"required":["schema","publisher_mode","output_profile","metering_rendition","storage"],"properties":{"schema":{"const":"rtmp-hls-session/v1"},"publisher_mode":{"enum":["gateway-relay","direct-publisher"]},"output_profile":{"type":"string"},"metering_rendition":{"type":"string"},"storage":{"type":"object"}}}`),
	}
}

func ValidateRunnerContractV1(value LiveRunnerContractDocumentV1) error {
	if value.CapabilityID != "video:transcode.live" || value.Protocol != PaidSessionProtocolV1 || len(value.DescriptorSchemas) != 1 || value.DescriptorSchemas[0] != RuntimeSchemaV1 || value.WorkUnit.Name != WorkUnitV1 || value.Metering != "runner-reported" || value.Heartbeat.IntervalSeconds == 0 {
		return errors.New("runner capability contract is invalid")
	}
	if value.Readiness.Type != "http-status" || value.Readiness.Path != "/ready" || value.Paths.Create != "/v1/sessions" || !strings.Contains(value.Paths.Status, "{id}") || !strings.Contains(value.Paths.Terminate, "{id}") || !json.Valid(value.SessionParamsSchema) {
		return errors.New("runner paths or parameter schema is invalid")
	}
	if value.Identity["provider"] == "" || value.SchemaVersions[PaidSessionProtocolV1] != PaidSessionVersionV1 || value.SchemaVersions[RuntimeSchemaV1] != RuntimeSchemaVersionV1 || value.SchemaVersions[SessionParamsSchemaV1] != SessionParamsVersionV1 {
		return errors.New("runner identity or schema versions are invalid")
	}
	return nil
}

// CalculateOutputSecondsV1 meters one named rendition's finalized segment
// timeline. It floors only after summing, so sub-second segments accumulate.
func CalculateOutputSecondsV1(segmentMicroseconds []uint64) (uint64, error) {
	var total uint64
	for _, duration := range segmentMicroseconds {
		var carry uint64
		total, carry = bits.Add64(total, duration, 0)
		if carry != 0 {
			return 0, errors.New("output timeline overflow")
		}
	}
	return total / 1_000_000, nil
}

func CustomerRuntimeV1(value RuntimePublicV1) struct {
	RTMPURL string `json:"rtmp_url"`
	HLSURL  string `json:"hls_url"`
} {
	return struct {
		RTMPURL string `json:"rtmp_url"`
		HLSURL  string `json:"hls_url"`
	}{RTMPURL: value.RTMPURL, HLSURL: value.HLSURL}
}

func CreateFingerprintV1(value RunnerCreateRequestV1) (string, error) { return canonicalHashV1(value) }
func KeyIssueFingerprintV1(value StreamKeyIssueRequestV1) (string, error) {
	return canonicalHashV1(value)
}

func DecodeStrictV1(reader io.Reader, value any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}

func validateStorageV1(value StorageV1) error {
	switch value.Kind {
	case "runner-local":
		if value.S3 != nil {
			return errors.New("runner-local storage cannot include s3 credentials")
		}
	case "s3":
		if value.S3 == nil {
			return errors.New("s3 storage credentials are required")
		}
		s3 := value.S3
		if err := validateHTTPURL(s3.Endpoint); err != nil || s3.Region == "" || s3.Bucket == "" || s3.Prefix == "" || strings.Contains(s3.Prefix, "..") || strings.HasPrefix(s3.Prefix, "/") || strings.ContainsAny(s3.Prefix, "?\\\r\n") || s3.AccessKeyID == "" || s3.SecretAccessKey == "" || s3.SessionToken == "" {
			return errors.New("s3 session storage is invalid")
		}
	default:
		return errors.New("storage kind is invalid")
	}
	return nil
}

func validStateV1(value string) bool {
	return value == "active" || value == "ended" || value == "failed"
}
func validCloseReasonV1(value string) bool {
	_, ok := closeReasonsV1[value]
	return ok
}

func validOutputStateV1(value string) bool {
	return value == OutputStateWaitingV1 || value == OutputStateProducingV1 || value == OutputStateStalledV1
}

func validLadderFailureCodeV1(value string, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	_, ok := ladderFailureCodesV1[value]
	return ok
}
func validateHTTPURL(raw string) error { return validateURLScheme(raw, "http", "https") }
func validateURLScheme(raw string, schemes ...string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return errors.New("URL is invalid")
	}
	for _, scheme := range schemes {
		if parsed.Scheme == scheme {
			return nil
		}
	}
	return errors.New("URL scheme is invalid")
}
func canonicalHashV1(value any) (string, error) {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes.TrimSuffix(body.Bytes(), []byte("\n")))
	return hex.EncodeToString(sum[:]), nil
}
