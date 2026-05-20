package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	RunnerAddr          string
	PublicHost          string
	TempDir             string
	HLSBasePath         string
	HLSWindowSegments   int
	DefaultPreset       string
	PresetsFile         string
	BrokerToken         string
	RTMPHost            string
	RTMPListenHost      string
	RTMPPortStart       int
	RTMPPortEnd         int
	SessionReadyTimeout time.Duration
	SessionNoPublishTTL time.Duration
	SessionIdleTTL      time.Duration
	CallbackTimeout     time.Duration
	CallbackInterval    time.Duration
	UsageTickInterval   time.Duration
	FFmpegBin           string
}

func loadConfig() config {
	return config{
		RunnerAddr:          env("RUNNER_ADDR", ":8080"),
		PublicHost:          env("PUBLIC_HOST", "127.0.0.1:8080"),
		TempDir:             env("TEMP_DIR", "/tmp/live"),
		HLSBasePath:         env("HLS_BASE_PATH", "/_hls"),
		HLSWindowSegments:   envInt("HLS_WINDOW_SEGMENTS", 6),
		DefaultPreset:       env("DEFAULT_LIVE_PRESET", "default"),
		PresetsFile:         env("PRESETS_FILE", ""),
		BrokerToken:         env("BROKER_AUTH_TOKEN", ""),
		RTMPHost:            env("RTMP_PUBLIC_HOST", "127.0.0.1"),
		RTMPListenHost:      env("RTMP_LISTEN_HOST", "0.0.0.0"),
		RTMPPortStart:       envInt("RTMP_PORT_START", 19350),
		RTMPPortEnd:         envInt("RTMP_PORT_END", 19449),
		SessionReadyTimeout: envDuration("SESSION_READY_TIMEOUT", 5*time.Second),
		SessionNoPublishTTL: envDuration("SESSION_NO_PUBLISH_TTL", 2*time.Minute),
		SessionIdleTTL:      envDuration("SESSION_IDLE_TTL", 30*time.Second),
		CallbackTimeout:     envDuration("BROKER_CALLBACK_TIMEOUT", 5*time.Second),
		CallbackInterval:    envDuration("SESSION_HEARTBEAT_INTERVAL", 10*time.Second),
		UsageTickInterval:   envDuration("SESSION_USAGE_TICK_INTERVAL", 5*time.Second),
		FFmpegBin:           env("FFMPEG_BIN", "ffmpeg"),
	}
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
