package main

import (
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

type liveSessionRequest struct {
	BrokerSessionID  string                `json:"broker_session_id"`
	WorkID           string                `json:"work_id"`
	CapabilityID     string                `json:"capability_id"`
	OfferingID       string                `json:"offering_id"`
	SessionParams    liveSessionParams     `json:"session_params"`
	BrokerCallbacks  brokerCallbackConfig  `json:"broker_callbacks"`
	OutputCredential *s3OutputCredential   `json:"output_credential,omitempty"`
	IngestAccept     *liveIngestAcceptance `json:"ingest_accept,omitempty"`
}

type liveSessionParams struct {
	Name               string            `json:"name,omitempty"`
	Preset             string            `json:"preset,omitempty"`
	Ladder             *liveLadder       `json:"ladder,omitempty"`
	IdleTimeoutSeconds int               `json:"idle_timeout_seconds,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
}

type liveLadder struct {
	Rungs []liveRung `json:"rungs"`
}

type liveRung struct {
	Name        string `json:"name"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	BitrateKbps int    `json:"bitrate_kbps,omitempty"`
	Passthrough bool   `json:"passthrough,omitempty"`
}

type brokerCallbackConfig struct {
	EventURL  string `json:"event_url"`
	AuthToken string `json:"auth_token"`
}

type s3OutputCredential struct {
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	KeyPrefix       string `json:"key_prefix"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
	ExpiresAt       string `json:"expires_at"`
}

type liveIngestAcceptance struct {
	StreamKey string `json:"stream_key"`
}

type ingestMediaResponse struct {
	RTMPURL   string `json:"rtmp_url,omitempty"`
	StreamKey string `json:"stream_key,omitempty"`
}

type playbackMediaResponse struct {
	HLSURL string `json:"hls_url,omitempty"`
}

type mediaResponse struct {
	Ingest   ingestMediaResponse   `json:"ingest,omitempty"`
	Playback playbackMediaResponse `json:"playback,omitempty"`
}

type createSessionResponse struct {
	RunnerSessionID  string        `json:"runner_session_id"`
	State            sessionState  `json:"state"`
	PrivateIngestURL string        `json:"private_ingest_url,omitempty"`
	Media            mediaResponse `json:"media,omitempty"`
	CreatedAt        string        `json:"created_at"`
}

type getSessionResponse struct {
	RunnerSessionID string                   `json:"runner_session_id"`
	BrokerSessionID string                   `json:"broker_session_id"`
	State           sessionState             `json:"state"`
	Ingest          liveIngestStatusResponse `json:"ingest"`
	Output          liveOutputStatusResponse `json:"output"`
	StartedAt       *string                  `json:"started_at,omitempty"`
	LastPacketAt    *string                  `json:"last_packet_at,omitempty"`
	LastHeartbeatAt *string                  `json:"last_heartbeat_at,omitempty"`
	CloseReason     *string                  `json:"close_reason,omitempty"`
	EndedAt         *string                  `json:"ended_at,omitempty"`
	UsageTotal      uint64                   `json:"usage_total"`
	UsageUnit       string                   `json:"usage_unit"`
}

type liveIngestStatusResponse struct {
	Mode               sessionMode `json:"mode"`
	ListenerBound      bool        `json:"listener_bound"`
	Authenticated      bool        `json:"authenticated"`
	ConnectedPublisher bool        `json:"connected_publisher"`
	StreamKeySuffix    string      `json:"stream_key_suffix,omitempty"`
	LastPacketAt       *string     `json:"last_packet_at,omitempty"`
}

type liveOutputStatusResponse struct {
	Mode              outputMode `json:"mode"`
	TargetPrefix      string     `json:"target_prefix,omitempty"`
	LastManifestPutAt *string    `json:"last_manifest_put_at,omitempty"`
	LastSegmentPutAt  *string    `json:"last_segment_put_at,omitempty"`
	PutSuccessCount   uint64     `json:"put_success_count"`
	PutFailureCount   uint64     `json:"put_failure_count"`
	LastPutError      *string    `json:"last_put_error,omitempty"`
}

type deleteSessionRequest struct {
	Reason string `json:"reason"`
}

type deleteSessionResponse struct {
	RunnerSessionID string       `json:"runner_session_id"`
	State           sessionState `json:"state"`
	CloseReason     string       `json:"close_reason"`
	EndedAt         string       `json:"ended_at"`
}

type eventEnvelope struct {
	BrokerSessionID string         `json:"broker_session_id"`
	RunnerSessionID string         `json:"runner_session_id"`
	EventID         string         `json:"event_id"`
	Sequence        uint64         `json:"sequence"`
	EventType       string         `json:"event_type"`
	EventTime       string         `json:"event_time"`
	State           sessionState   `json:"state"`
	Usage           eventUsage     `json:"usage"`
	CloseReason     *string        `json:"close_reason"`
	Details         map[string]any `json:"details"`
}

type eventUsage struct {
	Unit  string `json:"unit"`
	Delta uint64 `json:"delta"`
	Total uint64 `json:"total"`
}

type sessionState string

const (
	stateProvisioning sessionState = "provisioning"
	stateReady        sessionState = "ready"
	statePublishing   sessionState = "publishing"
	stateStalled      sessionState = "stalled"
	stateEnding       sessionState = "ending"
	stateEnded        sessionState = "ended"
	stateFailed       sessionState = "failed"
)

type sessionMode string

const (
	modeLocalHLSServe sessionMode = "local_hls_serve"
	modeGatewayIngest sessionMode = "gateway_ingest"
)

type outputMode string

const (
	outputModeLocalHLS outputMode = "local_hls"
	outputModeS3Push   outputMode = "s3_push"
)

type liveIngestStatus struct {
	Mode               sessionMode
	ListenerBound      bool
	Authenticated      bool
	ConnectedPublisher bool
	StreamKeySuffix    string
	LastPacketAt       time.Time
}

type liveOutputStatus struct {
	Mode              outputMode
	TargetPrefix      string
	LastManifestPutAt time.Time
	LastSegmentPutAt  time.Time
	PutSuccessCount   uint64
	PutFailureCount   uint64
	LastPutError      string
}

type sessionRecord struct {
	RunnerSessionID  string
	BrokerSessionID  string
	WorkID           string
	CapabilityID     string
	OfferingID       string
	Name             string
	State            sessionState
	Mode             sessionMode
	CloseReason      string
	RTMPURL          string
	PrivateIngestURL string
	StreamKey        string
	HLSURL           string
	RTMPPort         int
	CreatedAt        time.Time
	StartedAt        time.Time
	LastPacketAt     time.Time
	EndedAt          time.Time
	Terminated       bool
	OutputDir        string
	Preset           transcode.ABRPreset
	Callbacks        brokerCallbackConfig
	IdleTimeout      time.Duration
	OutputCredential *s3OutputCredential
	Ingest           liveIngestStatus
	Output           liveOutputStatus
}

type buildRuntime struct {
	Args      []string
	ListenURL string
	OutputDir string
	MasterURL string
	UsageUnit string
	CreatedAt time.Time
}
