package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	RunnerAddr                string
	IngestPublicHost          string
	SharedIngestAddr          string
	TempDir                   string
	HLSWindowSegments         int
	DefaultPreset             string
	PresetsFile               string
	BrokerToken               string
	SessionNoPublishTTL       time.Duration
	SessionIdleTTL            time.Duration
	CallbackTimeout           time.Duration
	CallbackInterval          time.Duration
	UsageTickInterval         time.Duration
	OutputSyncInterval        time.Duration
	OutputFailureThreshold    int
	OutputSyncUnsignedPayload bool
	FFmpegBin                 string
}

func loadConfig() config {
	return config{
		RunnerAddr:                env("RUNNER_ADDR", ":8080"),
		IngestPublicHost:          env("RUNNER_INGEST_PUBLIC_HOST", "127.0.0.1"),
		SharedIngestAddr:          env("RUNNER_SHARED_INGEST_ADDR", ":1935"),
		TempDir:                   env("TEMP_DIR", "/tmp/live"),
		HLSWindowSegments:         envInt("HLS_WINDOW_SEGMENTS", 6),
		DefaultPreset:             env("DEFAULT_LIVE_PRESET", "default"),
		PresetsFile:               env("PRESETS_FILE", ""),
		BrokerToken:               env("BROKER_AUTH_TOKEN", ""),
		SessionNoPublishTTL:       envDuration("SESSION_NO_PUBLISH_TTL", 2*time.Minute),
		SessionIdleTTL:            envDuration("SESSION_IDLE_TTL", 30*time.Second),
		CallbackTimeout:           envDuration("BROKER_CALLBACK_TIMEOUT", 5*time.Second),
		CallbackInterval:          envDuration("SESSION_HEARTBEAT_INTERVAL", 10*time.Second),
		UsageTickInterval:         envDuration("SESSION_USAGE_TICK_INTERVAL", 5*time.Second),
		OutputSyncInterval:        envDuration("OUTPUT_SYNC_INTERVAL", 250*time.Millisecond),
		OutputFailureThreshold:    envInt("OUTPUT_FAILURE_THRESHOLD", 3),
		OutputSyncUnsignedPayload: envBool("OUTPUT_SYNC_UNSIGNED_PAYLOAD", false),
		FFmpegBin:                 env("FFMPEG_BIN", "ffmpeg"),
	}
}

func (c config) logFields() string {
	return fmt.Sprintf(
		"runner_addr=%s ingest_public_host=%s shared_ingest_addr=%s temp_dir=%s hls_window_segments=%d default_preset=%s presets_file_set=%t broker_token_set=%t session_no_publish_ttl=%s session_idle_ttl=%s callback_timeout=%s callback_interval=%s usage_tick_interval=%s output_sync_interval=%s output_failure_threshold=%d output_sync_unsigned_payload=%t ffmpeg_bin=%s",
		c.RunnerAddr,
		c.IngestPublicHost,
		c.SharedIngestAddr,
		c.TempDir,
		c.HLSWindowSegments,
		c.DefaultPreset,
		strings.TrimSpace(c.PresetsFile) != "",
		strings.TrimSpace(c.BrokerToken) != "",
		c.SessionNoPublishTTL,
		c.SessionIdleTTL,
		c.CallbackTimeout,
		c.CallbackInterval,
		c.UsageTickInterval,
		c.OutputSyncInterval,
		c.OutputFailureThreshold,
		c.OutputSyncUnsignedPayload,
		c.FFmpegBin,
	)
}

func (c config) sharedIngestPort() string {
	if c.SharedIngestAddr == "" {
		return "1935"
	}
	_, port, err := net.SplitHostPort(c.SharedIngestAddr)
	if err == nil && port != "" {
		return port
	}
	if strings.HasPrefix(c.SharedIngestAddr, ":") {
		return strings.TrimPrefix(c.SharedIngestAddr, ":")
	}
	return "1935"
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}
