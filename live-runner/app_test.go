package liverunner

import (
	"bytes"
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

func TestLoadLiveRunnerConfigRequiresSecretsAndPublicCoordinates(t *testing.T) {
	values := map[string]string{
		"LIVE_RUNNER_MASTER_KEY":           base64.StdEncoding.EncodeToString([]byte(strings.Repeat("m", 32))),
		"LIVE_RUNNER_BROKER_TOKEN":         strings.Repeat("b", 32),
		"LIVE_RUNNER_INTERNAL_MEDIA_TOKEN": strings.Repeat("i", 32),
		"LIVEPEER_PUBLIC_RTMP_URL":         "rtmps://runner.example:1936",
		"LIVEPEER_PUBLIC_URL":              "https://runner.example/r/live-runner",
		"LIVE_RUNNER_PRESETS_FILE":         "/etc/live-runner/presets.yaml",
	}
	config, err := LoadLiveRunnerConfigV1(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if config.ListenAddress != ":8080" || config.MetricsAddress != "127.0.0.1:9090" || config.MediaMTX.APIAddress != "127.0.0.1:9997" || config.MaxConcurrent != 0 || config.CallbackPoll != 250*time.Millisecond || config.CallbackRetryInitial != 500*time.Millisecond || config.CallbackRetryMaximum != 30*time.Second || len(config.MasterKey) != 32 || config.HardwareTarget != "auto" {
		t.Fatal("valid configuration did not produce the expected safe defaults")
	}
	if config.OutputStallAfter != 20*time.Second || config.OutputFailAfter != time.Minute || config.RestartInitial != 250*time.Millisecond || config.RestartMaximum != 5*time.Second || config.RestartLimit != 5 {
		t.Fatalf("unexpected output health defaults: %#v", config)
	}
	if config.PublicRTMPBase != "rtmps://runner.example:1936" || config.PublicHTTPBase != "https://runner.example/r/live-runner" {
		t.Fatalf("public origins were not preserved: %#v", config)
	}
	// The broker token is optional: the broker attaches over the agent's
	// tunnel and presents no bearer. Absent is fine; short is not.
	delete(values, "LIVE_RUNNER_BROKER_TOKEN")
	config, err = LoadLiveRunnerConfigV1(func(name string) string { return values[name] })
	if err != nil || config.BrokerToken != "" {
		t.Fatalf("absent broker token should load with auth disabled: err=%v token=%q", err, config.BrokerToken)
	}
	values["LIVE_RUNNER_BROKER_TOKEN"] = "short"
	if _, err := LoadLiveRunnerConfigV1(func(name string) string { return values[name] }); err == nil || strings.Contains(err.Error(), "short") {
		t.Fatalf("short broker token error=%v", err)
	}
}

func TestLoadLiveRunnerConfigDoesNotAcceptDeletedPublicVariables(t *testing.T) {
	values := map[string]string{
		"LIVE_RUNNER_MASTER_KEY":           base64.StdEncoding.EncodeToString([]byte(strings.Repeat("m", 32))),
		"LIVE_RUNNER_BROKER_TOKEN":         strings.Repeat("b", 32),
		"LIVE_RUNNER_INTERNAL_MEDIA_TOKEN": strings.Repeat("i", 32),
		"LIVE_RUNNER_PUBLIC_RTMP_URL":      "rtmps://legacy.example/ingest",
		"LIVE_RUNNER_PUBLIC_HLS_BASE":      "https://legacy.example",
		"LIVE_RUNNER_PUBLIC_API_BASE":      "https://legacy.example",
		"LIVE_RUNNER_PRESETS_FILE":         "/etc/live-runner/presets.yaml",
	}
	_, err := LoadLiveRunnerConfigV1(func(name string) string { return values[name] })
	if err == nil || !strings.Contains(err.Error(), "LIVEPEER_PUBLIC_RTMP_URL") {
		t.Fatalf("deleted public variables were accepted: %v", err)
	}
}

func TestPackagedLivePresetsSupportSoftwareFallback(t *testing.T) {
	body, err := os.ReadFile("presets.yaml")
	if err != nil {
		t.Fatal(err)
	}
	presets, err := transcode.LoadABRPresetsFromBytes(body)
	if err != nil {
		t.Fatal(err)
	}
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x79}, 32))
	runtime, err := NewLiveRuntimeCoordinatorV1(store, fakePublisherWaiterV1{ready: make(chan struct{})}, &fakeLiveLauncherV1{}, noopLiveMeterV1{}, presets, transcode.HWProfile{}, "rtmp://127.0.0.1:1935", strings.Repeat("i", 32), time.Millisecond, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := readStrictFixtureV1[RunnerCreateRequestV1](t, "create-request.json")
	request.SessionParams.OutputProfile = "live-standard"
	request.SessionParams.MeteringRendition = "720p"
	if err := runtime.ValidateSession(request); err != nil {
		t.Fatalf("packaged software live preset rejected: %v", err)
	}
}

func TestLoadLiveRunnerConfigRejectsMalformedValues(t *testing.T) {
	values := map[string]string{"LIVE_RUNNER_MASTER_KEY": "not-base64"}
	if _, err := LoadLiveRunnerConfigV1(func(name string) string { return values[name] }); err == nil {
		t.Fatal("malformed master key was accepted")
	}
}

func TestLoadLiveRunnerConfigValidatesOutputHealthPolicy(t *testing.T) {
	values := map[string]string{
		"LIVE_RUNNER_MASTER_KEY":            base64.StdEncoding.EncodeToString([]byte(strings.Repeat("m", 32))),
		"LIVE_RUNNER_INTERNAL_MEDIA_TOKEN":  strings.Repeat("i", 32),
		"LIVEPEER_PUBLIC_RTMP_URL":          "rtmps://runner.example:1936",
		"LIVEPEER_PUBLIC_URL":               "https://runner.example",
		"LIVE_RUNNER_PRESETS_FILE":          "/etc/live-runner/presets.yaml",
		"LIVE_RUNNER_OUTPUT_STALL_DEADLINE": "30s",
		"LIVE_RUNNER_OUTPUT_FAIL_DEADLINE":  "20s",
	}
	if _, err := LoadLiveRunnerConfigV1(func(name string) string { return values[name] }); err == nil || !strings.Contains(err.Error(), "OUTPUT_FAIL_DEADLINE") {
		t.Fatalf("invalid output deadline policy error=%v", err)
	}
	values["LIVE_RUNNER_OUTPUT_FAIL_DEADLINE"] = "90s"
	values["LIVE_RUNNER_LADDER_RESTART_LIMIT"] = "0"
	if _, err := LoadLiveRunnerConfigV1(func(name string) string { return values[name] }); err == nil || !strings.Contains(err.Error(), "RESTART_LIMIT") {
		t.Fatalf("invalid restart limit error=%v", err)
	}
}

func TestLoadLiveRunnerConfigRequiresPrivateMetricsAndValidCallbackBackoff(t *testing.T) {
	values := map[string]string{
		"LIVE_RUNNER_MASTER_KEY":           base64.StdEncoding.EncodeToString([]byte(strings.Repeat("m", 32))),
		"LIVE_RUNNER_INTERNAL_MEDIA_TOKEN": strings.Repeat("i", 32),
		"LIVEPEER_PUBLIC_RTMP_URL":         "rtmps://runner.example:1936",
		"LIVEPEER_PUBLIC_URL":              "https://runner.example",
		"LIVE_RUNNER_PRESETS_FILE":         "/etc/live-runner/presets.yaml",
		"LIVE_RUNNER_METRICS_ADDR":         "0.0.0.0:9090",
	}
	if _, err := LoadLiveRunnerConfigV1(func(name string) string { return values[name] }); err == nil {
		t.Fatal("public runner metrics address was accepted")
	}
	values["LIVE_RUNNER_METRICS_ADDR"] = "127.0.0.1:9090"
	values["LIVE_RUNNER_CALLBACK_RETRY_INITIAL"] = "5s"
	values["LIVE_RUNNER_CALLBACK_RETRY_MAX"] = "1s"
	if _, err := LoadLiveRunnerConfigV1(func(name string) string { return values[name] }); err == nil {
		t.Fatal("inverted callback retry bounds were accepted")
	}
}

func TestLoadLiveRunnerConfigRejectsUnknownHardwareTarget(t *testing.T) {
	values := map[string]string{
		"LIVE_RUNNER_MASTER_KEY":           base64.StdEncoding.EncodeToString([]byte(strings.Repeat("m", 32))),
		"LIVE_RUNNER_BROKER_TOKEN":         strings.Repeat("b", 32),
		"LIVE_RUNNER_INTERNAL_MEDIA_TOKEN": strings.Repeat("i", 32),
		"LIVEPEER_PUBLIC_RTMP_URL":         "rtmps://runner.example:1936",
		"LIVEPEER_PUBLIC_URL":              "https://runner.example/r/live-runner",
		"LIVE_RUNNER_PRESETS_FILE":         "/etc/live-runner/presets.yaml",
		"LIVE_RUNNER_HARDWARE":             "universal-gpu",
	}
	if _, err := LoadLiveRunnerConfigV1(func(name string) string { return values[name] }); err == nil || !strings.Contains(err.Error(), "LIVE_RUNNER_HARDWARE") {
		t.Fatalf("unknown hardware target was accepted: %v", err)
	}
}

func TestResolveLiveHardwareRequiresAdvertisedVendor(t *testing.T) {
	nvidia := transcode.HWProfile{GPUName: "GeForce GTX 1080", Vendor: transcode.VendorNVIDIA, Encoders: []string{"h264_nvenc"}}
	got, err := resolveLiveHardwareV1("nvidia", func() transcode.HWProfile { return nvidia })
	if err != nil || got.Vendor != transcode.VendorNVIDIA {
		t.Fatalf("NVIDIA hardware rejected: got=%#v err=%v", got, err)
	}
	if _, err := resolveLiveHardwareV1("amd", func() transcode.HWProfile { return nvidia }); err == nil || !strings.Contains(err.Error(), "requires amd") {
		t.Fatalf("mismatched GPU was accepted: %v", err)
	}
	if _, err := resolveLiveHardwareV1("intel", func() transcode.HWProfile { return transcode.HWProfile{} }); err == nil || !strings.Contains(err.Error(), "requires intel") {
		t.Fatalf("missing GPU was accepted: %v", err)
	}
	if _, err := resolveLiveHardwareV1("intel", func() transcode.HWProfile {
		return transcode.HWProfile{GPUName: "Intel GPU", Vendor: transcode.VendorIntel, Encoders: []string{"h264_vaapi"}}
	}); err == nil || !strings.Contains(err.Error(), "h264_qsv") {
		t.Fatalf("missing target encoder was accepted: %v", err)
	}
}

func TestResolveLiveHardwareCPUIsDeliberate(t *testing.T) {
	detected := false
	got, err := resolveLiveHardwareV1("cpu", func() transcode.HWProfile {
		detected = true
		return transcode.HWProfile{GPUName: "unexpected", Vendor: transcode.VendorNVIDIA, Encoders: []string{"h264_nvenc"}}
	})
	if err != nil || detected || got.IsGPUAvailable() {
		t.Fatalf("CPU target did not disable hardware: got=%#v detected=%v err=%v", got, detected, err)
	}
}
