package liverunner

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

type LiveRunnerConfigV1 struct {
	ListenAddress        string
	MetricsAddress       string
	StateDirectory       string
	MasterKey            []byte
	BrokerToken          string
	InternalToken        string
	PublicRTMPBase       string
	PublicHTTPBase       string
	PresetsFile          string
	MediaMTXBinary       string
	MediaMTXConfig       string
	MediaMTX             MediaMTXConfigV1
	RouterRTMPBase       string
	MaxConcurrent        int
	StartupTimeout       time.Duration
	ShutdownTimeout      time.Duration
	RouterPoll           time.Duration
	MeterPoll            time.Duration
	HeartbeatEvery       time.Duration
	CallbackPoll         time.Duration
	CallbackRetryInitial time.Duration
	CallbackRetryMaximum time.Duration
	RequestTimeout       time.Duration
	HLSHeaderTimeout     time.Duration
	GrantTTL             time.Duration
	StreamKeyTTL         time.Duration
	HardwareTarget       string
	OutputStallAfter     time.Duration
	OutputFailAfter      time.Duration
	InitialPublishWait   time.Duration
	ReconnectGrace       time.Duration
	RestartInitial       time.Duration
	RestartMaximum       time.Duration
	RestartWindow        time.Duration
	RestartLimit         int
	GPUAdmissionLock     string
}

func LoadLiveRunnerConfigV1(getenv func(string) string) (LiveRunnerConfigV1, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	required := func(name string) (string, error) {
		value := getenv(name)
		if value == "" {
			return "", fmt.Errorf("%s is required", name)
		}
		return value, nil
	}
	masterEncoded, err := required("LIVE_RUNNER_MASTER_KEY")
	if err != nil {
		return LiveRunnerConfigV1{}, err
	}
	masterKey, err := base64.StdEncoding.DecodeString(masterEncoded)
	if err != nil || len(masterKey) != 32 {
		return LiveRunnerConfigV1{}, errors.New("LIVE_RUNNER_MASTER_KEY must be base64 for exactly 32 bytes")
	}
	// Optional. Under the attach model the broker reaches this runner over
	// the pool member agent's tunnel and presents no credential of its own
	// — the tunnel is the trust boundary (runner-attach §7) — so a token
	// here would only ever reject the broker. Set it to gate the session
	// routes when something other than an agent can reach this port.
	brokerToken := getenv("LIVE_RUNNER_BROKER_TOKEN")
	if brokerToken != "" && len(brokerToken) < 32 {
		return LiveRunnerConfigV1{}, errors.New("LIVE_RUNNER_BROKER_TOKEN must contain at least 32 characters when set")
	}
	internalToken, err := required("LIVE_RUNNER_INTERNAL_MEDIA_TOKEN")
	if err != nil || len(internalToken) < 32 {
		return LiveRunnerConfigV1{}, errors.New("LIVE_RUNNER_INTERNAL_MEDIA_TOKEN must contain at least 32 characters")
	}
	publicRTMP, err := required("LIVEPEER_PUBLIC_RTMP_URL")
	if err != nil {
		return LiveRunnerConfigV1{}, err
	}
	publicHTTP, err := required("LIVEPEER_PUBLIC_URL")
	if err != nil {
		return LiveRunnerConfigV1{}, err
	}
	presetsFile, err := required("LIVE_RUNNER_PRESETS_FILE")
	if err != nil {
		return LiveRunnerConfigV1{}, err
	}
	integer := func(name string, fallback int) (int, error) {
		value := getenv(name)
		if value == "" {
			return fallback, nil
		}
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			return 0, fmt.Errorf("%s must be a non-negative integer", name)
		}
		return parsed, nil
	}
	maxConcurrent, err := integer("LIVE_RUNNER_MAX_CONCURRENT", 0)
	if err != nil {
		return LiveRunnerConfigV1{}, err
	}
	restartLimit, err := integer("LIVE_RUNNER_LADDER_RESTART_LIMIT", 5)
	if err != nil || restartLimit == 0 {
		return LiveRunnerConfigV1{}, errors.New("LIVE_RUNNER_LADDER_RESTART_LIMIT must be positive")
	}
	duration := func(name string, fallback time.Duration) (time.Duration, error) {
		value := getenv(name)
		if value == "" {
			return fallback, nil
		}
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			return 0, fmt.Errorf("%s must be a positive duration", name)
		}
		return parsed, nil
	}
	stallAfter, err := duration("LIVE_RUNNER_OUTPUT_STALL_DEADLINE", 20*time.Second)
	if err != nil {
		return LiveRunnerConfigV1{}, err
	}
	failAfter, err := duration("LIVE_RUNNER_OUTPUT_FAIL_DEADLINE", 60*time.Second)
	if err != nil || failAfter <= stallAfter {
		return LiveRunnerConfigV1{}, errors.New("LIVE_RUNNER_OUTPUT_FAIL_DEADLINE must exceed LIVE_RUNNER_OUTPUT_STALL_DEADLINE")
	}
	initialPublishWait, err := duration("LIVE_RUNNER_INITIAL_PUBLISH_TIMEOUT", 5*time.Minute)
	if err != nil {
		return LiveRunnerConfigV1{}, err
	}
	reconnectGrace, err := duration("LIVE_RUNNER_RECONNECT_GRACE", 2*time.Minute)
	if err != nil {
		return LiveRunnerConfigV1{}, err
	}
	restartInitial, err := duration("LIVE_RUNNER_LADDER_RESTART_INITIAL", 250*time.Millisecond)
	if err != nil {
		return LiveRunnerConfigV1{}, err
	}
	restartMaximum, err := duration("LIVE_RUNNER_LADDER_RESTART_MAX", 5*time.Second)
	if err != nil || restartMaximum < restartInitial {
		return LiveRunnerConfigV1{}, errors.New("LIVE_RUNNER_LADDER_RESTART_MAX must not be shorter than LIVE_RUNNER_LADDER_RESTART_INITIAL")
	}
	restartWindow, err := duration("LIVE_RUNNER_LADDER_FAILURE_WINDOW", time.Minute)
	if err != nil {
		return LiveRunnerConfigV1{}, err
	}
	callbackRetryInitial, err := duration("LIVE_RUNNER_CALLBACK_RETRY_INITIAL", 500*time.Millisecond)
	if err != nil {
		return LiveRunnerConfigV1{}, err
	}
	callbackRetryMaximum, err := duration("LIVE_RUNNER_CALLBACK_RETRY_MAX", 30*time.Second)
	if err != nil || callbackRetryMaximum < callbackRetryInitial {
		return LiveRunnerConfigV1{}, errors.New("LIVE_RUNNER_CALLBACK_RETRY_MAX must not be shorter than LIVE_RUNNER_CALLBACK_RETRY_INITIAL")
	}
	hardwareTarget := valueOrV1(getenv("LIVE_RUNNER_HARDWARE"), "auto")
	switch hardwareTarget {
	case "auto", "cpu", string(transcode.VendorNVIDIA), string(transcode.VendorIntel), string(transcode.VendorAMD):
	default:
		return LiveRunnerConfigV1{}, errors.New("LIVE_RUNNER_HARDWARE must be one of auto, cpu, nvidia, intel, or amd")
	}
	config := LiveRunnerConfigV1{
		ListenAddress:  valueOrV1(getenv("LIVE_RUNNER_ADDR"), ":8080"),
		MetricsAddress: valueOrV1(getenv("LIVE_RUNNER_METRICS_ADDR"), "127.0.0.1:9090"),
		StateDirectory: valueOrV1(getenv("LIVE_RUNNER_STATE_DIR"), "/var/lib/live-runner"),
		MasterKey:      masterKey, BrokerToken: brokerToken, InternalToken: internalToken,
		PublicRTMPBase: publicRTMP, PublicHTTPBase: publicHTTP, PresetsFile: presetsFile,
		MediaMTXBinary: valueOrV1(getenv("LIVE_RUNNER_MEDIAMTX_BINARY"), "/usr/local/bin/mediamtx"),
		RouterRTMPBase: valueOrV1(getenv("LIVE_RUNNER_ROUTER_RTMP_BASE"), "rtmp://127.0.0.1:1935"),
		MaxConcurrent:  maxConcurrent, StartupTimeout: 30 * time.Second, ShutdownTimeout: 30 * time.Second,
		RouterPoll: 100 * time.Millisecond, MeterPoll: 250 * time.Millisecond, HeartbeatEvery: 4 * time.Second, CallbackPoll: 250 * time.Millisecond,
		RequestTimeout: 2 * time.Second, GrantTTL: time.Hour, StreamKeyTTL: 10 * time.Minute,
		HLSHeaderTimeout: 15 * time.Second,
		HardwareTarget:   hardwareTarget,
		OutputStallAfter: stallAfter, OutputFailAfter: failAfter,
		InitialPublishWait: initialPublishWait, ReconnectGrace: reconnectGrace,
		RestartInitial: restartInitial, RestartMaximum: restartMaximum, RestartWindow: restartWindow, RestartLimit: restartLimit,
		CallbackRetryInitial: callbackRetryInitial, CallbackRetryMaximum: callbackRetryMaximum,
		GPUAdmissionLock: getenv("GPU_ADMISSION_LOCK"),
	}
	if err := validateListenAddressV1(config.MetricsAddress, true); err != nil {
		return LiveRunnerConfigV1{}, fmt.Errorf("live runner metrics address: %w", err)
	}
	config.MediaMTXConfig = config.StateDirectory + "/mediamtx.yml"
	config.MediaMTX = DefaultMediaMTXConfigV1(valueOrV1(getenv("LIVE_RUNNER_MEDIAMTX_AUTH_URL"), "http://127.0.0.1:8080/internal/mediamtx/auth"))
	config.MediaMTX.RTMPAddress = valueOrV1(getenv("LIVE_RUNNER_MEDIAMTX_RTMP_ADDR"), config.MediaMTX.RTMPAddress)
	config.MediaMTX.HLSAddress = valueOrV1(getenv("LIVE_RUNNER_MEDIAMTX_HLS_ADDR"), config.MediaMTX.HLSAddress)
	config.MediaMTX.APIAddress = valueOrV1(getenv("LIVE_RUNNER_MEDIAMTX_API_ADDR"), config.MediaMTX.APIAddress)
	config.MediaMTX.MetricsAddress = valueOrV1(getenv("LIVE_RUNNER_MEDIAMTX_METRICS_ADDR"), config.MediaMTX.MetricsAddress)
	return config, nil
}

func RunLiveRunnerV1(ctx context.Context, config LiveRunnerConfigV1) error {
	presetBody, err := os.ReadFile(config.PresetsFile)
	if err != nil {
		return errors.New("read live presets failed")
	}
	presets, err := transcode.LoadABRPresetsFromBytes(presetBody)
	if err != nil {
		return errors.New("parse live presets failed")
	}
	hardware, err := resolveLiveHardwareV1(config.HardwareTarget, transcode.DetectGPU)
	if err != nil {
		return err
	}
	store, err := NewEncryptedFileSessionStoreV1(config.StateDirectory+"/sessions", config.MasterKey)
	if err != nil {
		return err
	}
	router, err := NewMediaRouterClientV1("http://"+config.MediaMTX.APIAddress, nil, config.RequestTimeout)
	if err != nil {
		return err
	}
	hls, err := NewHLSHandlerV1(store, presets, "http://"+config.MediaMTX.HLSAddress, nil, config.HLSHeaderTimeout)
	if err != nil {
		return err
	}
	meter, err := NewLiveOutputMeterV1(store, hls, router, config.MeterPoll, config.HeartbeatEvery, config.RequestTimeout, config.OutputStallAfter, config.OutputFailAfter)
	if err != nil {
		return err
	}
	meter.initialPublishWait = config.InitialPublishWait
	meter.reconnectGrace = config.ReconnectGrace
	dispatcher, err := NewCallbackDispatcherV1(store, nil, config.RequestTimeout)
	if err != nil {
		return err
	}
	metrics := &LiveRunnerMetricsV1{}
	meter.metrics = metrics
	dispatcher.retryInitial = config.CallbackRetryInitial
	dispatcher.retryMaximum = config.CallbackRetryMaximum
	dispatcher.rejectionCount = metrics.RecordCallbackRejected
	callbackWorker, err := NewCallbackWorkerV1(store, dispatcher, config.CallbackPoll)
	if err != nil {
		return err
	}
	gpuAdmission, err := transcode.NewGPUAdmissionGate(config.GPUAdmissionLock)
	if err != nil {
		return fmt.Errorf("initialize GPU admission: %w", err)
	}
	runtime, err := NewLiveRuntimeCoordinatorV1(store, router, FFmpegLiveLadderLauncherV1{}, meter, presets, hardware, config.RouterRTMPBase, config.InternalToken, config.RouterPoll, config.MaxConcurrent, gpuAdmission)
	if err != nil {
		return err
	}
	runtime.restartInitial = config.RestartInitial
	runtime.restartMax = config.RestartMaximum
	runtime.failureWindow = config.RestartWindow
	runtime.maxFailures = uint32(config.RestartLimit)
	runtime.metrics = metrics
	supervisor, err := NewMediaMTXSupervisorV1(config.MediaMTXBinary, config.MediaMTXConfig, config.MediaMTX, config.RequestTimeout, config.RouterPoll)
	if err != nil {
		return err
	}
	startup, cancelStartup := context.WithTimeout(ctx, config.StartupTimeout)
	err = supervisor.Start(startup)
	cancelStartup()
	if err != nil {
		return err
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), config.ShutdownTimeout)
		_ = supervisor.Stop(shutdown)
		cancel()
	}()
	if err := RecoverLiveSessionsV1(ctx, store, runtime, time.Now); err != nil {
		return err
	}
	callbackContext, cancelCallbacks := context.WithCancel(ctx)
	callbackExit := make(chan error, 1)
	go func() { callbackExit <- callbackWorker.Run(callbackContext) }()
	factory := RunnerResponseFactoryV1{PublicRTMPBase: config.PublicRTMPBase, PublicHTTPBase: config.PublicHTTPBase, GrantTTL: config.GrantTTL}
	if err := factory.Validate(); err != nil {
		cancelCallbacks()
		<-callbackExit
		return err
	}
	serverDefinition := &LiveRunnerServerV1{Store: store, Runtime: runtime, Factory: factory, BrokerToken: config.BrokerToken, KeyTTL: config.StreamKeyTTL, Ready: supervisor.Ready, HLS: hls}
	handler, err := serverDefinition.Handler(MediaMTXAuthorizerV1{Sessions: store, InternalTokenRoot: config.InternalToken})
	if err != nil {
		cancelCallbacks()
		<-callbackExit
		return err
	}
	httpServer := &http.Server{Addr: config.ListenAddress, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	metricsServer := &http.Server{Addr: config.MetricsAddress, Handler: metrics, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	httpExit := make(chan error, 1)
	go func() {
		err := httpServer.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		httpExit <- err
	}()
	metricsExit := make(chan error, 1)
	go func() {
		err := metricsServer.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		metricsExit <- err
	}()
	var runErr error
	callbackStopped := false
	select {
	case <-ctx.Done():
	case <-supervisor.Done():
		runErr = ErrMediaRouterExitedV1
	case runErr = <-callbackExit:
		callbackStopped = true
	case runErr = <-httpExit:
	case runErr = <-metricsExit:
	}
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), config.ShutdownTimeout)
	defer cancelShutdown()
	cancelCallbacks()
	serverErr := httpServer.Shutdown(shutdown)
	metricsErr := metricsServer.Shutdown(shutdown)
	runtimeErr := runtime.Shutdown(shutdown)
	var callbackErr error
	if !callbackStopped {
		select {
		case callbackErr = <-callbackExit:
		case <-shutdown.Done():
			callbackErr = shutdown.Err()
		}
	}
	mediaErr := supervisor.Stop(shutdown)
	return errors.Join(runErr, serverErr, metricsErr, runtimeErr, callbackErr, mediaErr)
}

func resolveLiveHardwareV1(target string, detect func() transcode.HWProfile) (transcode.HWProfile, error) {
	if target == "cpu" {
		return transcode.HWProfile{}, nil
	}
	hardware := detect()
	if target == "auto" {
		return hardware, nil
	}
	if !hardware.IsGPUAvailable() || string(hardware.Vendor) != target {
		return transcode.HWProfile{}, fmt.Errorf("live-runner image requires %s GPU acceleration, but that hardware was not detected", target)
	}
	requiredEncoder := map[string]string{
		string(transcode.VendorNVIDIA): "h264_nvenc",
		string(transcode.VendorIntel):  "h264_qsv",
		string(transcode.VendorAMD):    "h264_vaapi",
	}[target]
	if !hardware.HasEncoder(requiredEncoder) {
		return transcode.HWProfile{}, fmt.Errorf("live-runner image requires %s encoder %s, but it was not detected", target, requiredEncoder)
	}
	return hardware, nil
}

func valueOrV1(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
