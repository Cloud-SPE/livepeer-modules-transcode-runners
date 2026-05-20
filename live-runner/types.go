package main

import (
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

type liveSessionRequest struct {
	BrokerSessionID string               `json:"broker_session_id"`
	WorkID          string               `json:"work_id"`
	CapabilityID    string               `json:"capability_id"`
	OfferingID      string               `json:"offering_id"`
	SessionParams   liveSessionParams    `json:"session_params"`
	BrokerCallbacks brokerCallbackConfig `json:"broker_callbacks"`
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

type mediaResponse struct {
	Ingest struct {
		RTMPURL   string `json:"rtmp_url"`
		StreamKey string `json:"stream_key,omitempty"`
	} `json:"ingest"`
	Playback struct {
		HLSURL string `json:"hls_url"`
	} `json:"playback"`
}

type createSessionResponse struct {
	RunnerSessionID string        `json:"runner_session_id"`
	State           sessionState  `json:"state"`
	Media           mediaResponse `json:"media"`
	CreatedAt       string        `json:"created_at"`
}

type getSessionResponse struct {
	RunnerSessionID  string       `json:"runner_session_id"`
	BrokerSessionID  string       `json:"broker_session_id"`
	State            sessionState `json:"state"`
	StartedAt        *string      `json:"started_at,omitempty"`
	LastPacketAt     *string      `json:"last_packet_at,omitempty"`
	LastHeartbeatAt  *string      `json:"last_heartbeat_at,omitempty"`
	CloseReason      *string      `json:"close_reason,omitempty"`
	EndedAt          *string      `json:"ended_at,omitempty"`
	UsageTotal       uint64       `json:"usage_total"`
	UsageUnit        string       `json:"usage_unit"`
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
	stateEnding       sessionState = "ending"
	stateEnded        sessionState = "ended"
	stateFailed       sessionState = "failed"
)

type sessionRecord struct {
	RunnerSessionID string
	BrokerSessionID string
	WorkID          string
	CapabilityID    string
	OfferingID      string
	Name            string
	State           sessionState
	CloseReason     string
	RTMPURL         string
	StreamKey       string
	HLSURL          string
	RTMPPort        int
	CreatedAt       time.Time
	StartedAt       time.Time
	LastPacketAt    time.Time
	EndedAt         time.Time
	Terminated      bool
	OutputDir       string
	Preset          transcode.ABRPreset
	Callbacks       brokerCallbackConfig
	IdleTimeout     time.Duration
}

type buildRuntime struct {
	Args        []string
	ListenURL   string
	OutputDir   string
	MasterURL   string
	UsageUnit   string
	CreatedAt   time.Time
	ReadySignal string
}
