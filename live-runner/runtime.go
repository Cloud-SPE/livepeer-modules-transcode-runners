package liverunner

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

type MediaPublisherWaiterV1 interface {
	Path(context.Context, string) (MediaPathStatusV1, error)
	WaitForRTMPPublisher(context.Context, string, time.Duration) (MediaPathStatusV1, error)
	KickPublisher(context.Context, string) error
}

type LiveLadderProcessV1 interface {
	Wait() LiveLadderExitV1
}

type LiveLadderExitV1 struct {
	Code           string
	Err            error
	DiagnosticTail []string
}

type LiveLadderLauncherV1 interface {
	Start(context.Context, string, []transcode.LiveRTMPOutput, transcode.HWProfile, transcode.ProbeResult) (LiveLadderProcessV1, error)
}

type LiveSessionMeterV1 interface {
	Run(context.Context, SessionRecordV1, SessionSecretsV1) string
}

type FFmpegLiveLadderLauncherV1 struct{}

func (l FFmpegLiveLadderLauncherV1) Start(ctx context.Context, inputURL string, outputs []transcode.LiveRTMPOutput, hardware transcode.HWProfile, probe transcode.ProbeResult) (LiveLadderProcessV1, error) {
	command, err := transcode.LiveLadderCmdContext(ctx, inputURL, outputs, hardware, probe)
	if err != nil {
		return nil, err
	}
	command.Stdout = io.Discard
	// FFmpeg can include credential-bearing RTMP URLs in diagnostics. Runtime
	// health is reported through safe event codes, never raw process output.
	stderr, err := command.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	process := &execLiveLadderProcessV1{command: command, done: make(chan struct{})}
	go process.capture(stderr)
	return process, nil
}

type execLiveLadderProcessV1 struct {
	command *exec.Cmd
	done    chan struct{}
	mu      sync.Mutex
	tail    []string
}

func (p *execLiveLadderProcessV1) capture(reader io.Reader) {
	defer close(p.done)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := sanitizeFFmpegDiagnosticV1(scanner.Text())
		if line == "" || isFFmpegProgressLineV1(line) {
			continue
		}
		p.mu.Lock()
		p.tail = append(p.tail, line)
		if len(p.tail) > 12 {
			p.tail = append([]string(nil), p.tail[len(p.tail)-12:]...)
		}
		p.mu.Unlock()
	}
}

func (p *execLiveLadderProcessV1) Wait() LiveLadderExitV1 {
	err := p.command.Wait()
	<-p.done
	p.mu.Lock()
	tail := append([]string(nil), p.tail...)
	p.mu.Unlock()
	return LiveLadderExitV1{Code: classifyLiveLadderExitV1(err, tail), Err: err, DiagnosticTail: tail}
}

var liveDiagnosticURLV1 = regexp.MustCompile(`(?i)\b(?:rtmps?|https?)://[^\s"']+`)

func sanitizeFFmpegDiagnosticV1(line string) string {
	line = liveDiagnosticURLV1.ReplaceAllStringFunc(line, func(value string) string {
		if parsed, err := url.Parse(value); err == nil {
			return parsed.Scheme + "://[redacted]"
		}
		return "[redacted-url]"
	})
	line = strings.TrimSpace(line)
	if len(line) > 512 {
		line = line[:512]
	}
	return line
}

func isFFmpegProgressLineV1(line string) bool {
	key, _, ok := strings.Cut(line, "=")
	if !ok {
		return false
	}
	switch key {
	case "bitrate", "drop_frames", "dup_frames", "fps", "frame", "out_time", "out_time_ms", "out_time_us", "progress", "speed", "stream_0_0_q", "total_size":
		return true
	}
	return false
}

func classifyLiveLadderExitV1(err error, tail []string) string {
	joined := strings.ToLower(strings.Join(tail, "\n"))
	if err != nil {
		joined += "\n" + strings.ToLower(err.Error())
	}
	switch {
	case errors.Is(err, context.Canceled) || strings.Contains(joined, "signal: killed"):
		return "process_killed"
	case strings.Contains(joined, "error while opening encoder") || strings.Contains(joined, "no capable devices found") || strings.Contains(joined, "too many concurrent sessions"):
		return "encoder_init_failed"
	case strings.Contains(joined, "cannot load libcuda") || strings.Contains(joined, "device setup failed") || strings.Contains(joined, "failed to initialise vaapi") || strings.Contains(joined, "hardware accelerator failed"):
		return "hwaccel_init_failed"
	case strings.Contains(joined, "error opening input") || strings.Contains(joined, "connection refused") || strings.Contains(joined, "input/output error"):
		return "input_unavailable"
	case strings.Contains(joined, "error writing") || strings.Contains(joined, "broken pipe") || strings.Contains(joined, "server error"):
		return "output_rejected"
	default:
		return "unknown"
	}
}

type activeLiveSessionV1 struct {
	cancel   context.CancelFunc
	done     chan struct{}
	gpuLease *transcode.GPUAdmissionLease
}

type LiveRuntimeCoordinatorV1 struct {
	store             *EncryptedFileSessionStoreV1
	router            MediaPublisherWaiterV1
	launcher          LiveLadderLauncherV1
	meter             LiveSessionMeterV1
	presets           map[string]transcode.ABRPreset
	hardware          transcode.HWProfile
	routerRTMPBase    string
	internalTokenRoot string
	pollInterval      time.Duration
	restartInitial    time.Duration
	restartMax        time.Duration
	failureWindow     time.Duration
	maxFailures       uint32
	cleanupTimeout    time.Duration
	now               func() time.Time
	log               *slog.Logger
	metrics           *LiveRunnerMetricsV1
	gpuPressure       GPUPressureSamplerV1
	gpuAdmission      *transcode.GPUAdmissionGate
	capacity          chan struct{}

	mu       sync.Mutex
	sessions map[string]*activeLiveSessionV1
}

func NewLiveRuntimeCoordinatorV1(store *EncryptedFileSessionStoreV1, router MediaPublisherWaiterV1, launcher LiveLadderLauncherV1, meter LiveSessionMeterV1, presets []transcode.ABRPreset, hardware transcode.HWProfile, routerRTMPBase, internalTokenRoot string, pollInterval time.Duration, maxConcurrent int, admission *transcode.GPUAdmissionGate) (*LiveRuntimeCoordinatorV1, error) {
	if store == nil || router == nil || launcher == nil || meter == nil || len(presets) == 0 || len(internalTokenRoot) < 32 || pollInterval <= 0 || maxConcurrent < 0 {
		return nil, errors.New("live runtime dependencies are incomplete")
	}
	parsed, err := url.Parse(routerRTMPBase)
	if err != nil || parsed.Scheme != "rtmp" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("internal media router RTMP base is invalid")
	}
	byName := make(map[string]transcode.ABRPreset, len(presets))
	for _, preset := range presets {
		key := strings.ToLower(preset.Name)
		if key == "" || len(preset.Renditions) == 0 || preset.SegmentDuration <= 0 || preset.SegmentDuration > 10 {
			return nil, errors.New("live runtime preset is invalid")
		}
		if _, duplicate := byName[key]; duplicate {
			return nil, errors.New("live runtime preset names must be unique")
		}
		byName[key] = preset
	}
	var capacity chan struct{}
	if maxConcurrent > 0 {
		capacity = make(chan struct{}, maxConcurrent)
	}
	return &LiveRuntimeCoordinatorV1{
		store: store, router: router, launcher: launcher, meter: meter, presets: byName, hardware: hardware,
		routerRTMPBase: strings.TrimRight(routerRTMPBase, "/"), internalTokenRoot: internalTokenRoot,
		pollInterval: pollInterval, gpuAdmission: admission, capacity: capacity, sessions: make(map[string]*activeLiveSessionV1),
		restartInitial: 250 * time.Millisecond, restartMax: 5 * time.Second, failureWindow: time.Minute,
		maxFailures: 5, cleanupTimeout: 5 * time.Second, now: time.Now, log: slog.Default(), metrics: &LiveRunnerMetricsV1{},
		gpuPressure: NVIDIASMIPressureSamplerV1{Timeout: time.Second},
	}, nil
}

func (c *LiveRuntimeCoordinatorV1) ValidateSession(request RunnerCreateRequestV1) error {
	preset, ok := c.presets[strings.ToLower(request.SessionParams.OutputProfile)]
	if !ok {
		return errors.New("unknown live output profile")
	}
	meteringFound := false
	for _, rendition := range preset.Renditions {
		if rendition.Name == request.SessionParams.MeteringRendition {
			meteringFound = true
		}
		if rendition.Video != nil && !strings.EqualFold(rendition.Video.Codec, "h264") && !strings.EqualFold(rendition.Video.Codec, "avc") {
			return errors.New("live output profile contains a non-H264 rendition")
		}
		if !strings.EqualFold(rendition.Audio.Codec, "aac") {
			return errors.New("live output profile contains a non-AAC rendition")
		}
	}
	if !meteringFound {
		return errors.New("metering rendition is not in the live output profile")
	}
	return nil
}

func (c *LiveRuntimeCoordinatorV1) EnsureSession(_ context.Context, record SessionRecordV1, secrets SessionSecretsV1) error {
	if record.State != "active" || record.Stopping {
		return ErrSessionTerminalV1
	}
	if err := c.ValidateSession(secrets.CreateRequest); err != nil {
		return err
	}
	c.mu.Lock()
	if _, exists := c.sessions[record.RunnerSessionID]; exists {
		c.mu.Unlock()
		return nil
	}
	if !c.acquireLocal() {
		c.metrics.RecordGPUAdmissionRejected()
		c.mu.Unlock()
		return transcode.ErrGPUAdmissionCapacity
	}
	lease, err := c.gpuAdmission.Acquire(transcode.GPUAdmissionLive)
	if err != nil {
		c.releaseLocal()
		if errors.Is(err, transcode.ErrGPUAdmissionCapacity) {
			c.metrics.RecordGPUAdmissionRejected()
		}
		c.mu.Unlock()
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	active := &activeLiveSessionV1{cancel: cancel, done: make(chan struct{}), gpuLease: lease}
	c.sessions[record.RunnerSessionID] = active
	c.mu.Unlock()
	if record.LastSequence == 0 {
		details, err := outputHealthDetailsV1(record)
		if err != nil {
			cancel()
			c.remove(record.RunnerSessionID, active)
			_ = lease.Close()
			c.releaseLocal()
			return err
		}
		event := RunnerEventV1{
			EventID: record.RunnerSessionID + ":1", Sequence: 1, EventType: "session.started",
			EventTime: c.now().UTC().Format(time.RFC3339Nano), State: "active", Details: details,
		}
		if err := c.store.Advance(record.BrokerSessionID, event); err != nil {
			cancel()
			c.remove(record.RunnerSessionID, active)
			_ = lease.Close()
			c.releaseLocal()
			return err
		}
	}
	go c.run(ctx, active, record, secrets)
	return nil
}

func (c *LiveRuntimeCoordinatorV1) TerminateSession(ctx context.Context, record SessionRecordV1) error {
	ingestPath, err := IngestMediaPathV1(record.RunnerSessionID)
	if err != nil {
		return err
	}
	c.mu.Lock()
	active := c.sessions[record.RunnerSessionID]
	c.mu.Unlock()
	if active != nil {
		active.cancel()
	}
	kickErr := c.router.KickPublisher(ctx, ingestPath)
	if active == nil {
		return kickErr
	}
	select {
	case <-active.done:
		return kickErr
	case <-ctx.Done():
		return errors.Join(kickErr, ctx.Err())
	}
}

func (c *LiveRuntimeCoordinatorV1) ActivateStreamKey(ctx context.Context, record SessionRecordV1) error {
	ingestPath, err := IngestMediaPathV1(record.RunnerSessionID)
	if err != nil {
		return err
	}
	return c.router.KickPublisher(ctx, ingestPath)
}

func (c *LiveRuntimeCoordinatorV1) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	active := make([]*activeLiveSessionV1, 0, len(c.sessions))
	for _, session := range c.sessions {
		session.cancel()
		active = append(active, session)
	}
	c.mu.Unlock()
	for _, session := range active {
		select {
		case <-session.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (c *LiveRuntimeCoordinatorV1) run(ctx context.Context, active *activeLiveSessionV1, record SessionRecordV1, secrets SessionSecretsV1) {
	defer close(active.done)
	defer c.remove(record.RunnerSessionID, active)
	defer c.releaseLocal()
	defer active.gpuLease.Close()
	runContext, cancel := context.WithCancel(ctx)
	meterDone := make(chan string, 1)
	go func() {
		meterDone <- c.meter.Run(runContext, record, secrets)
		close(meterDone)
	}()
	defer func() {
		cancel()
		select {
		case <-meterDone:
		default:
			<-meterDone
		}
	}()
	ingestPath, _ := IngestMediaPathV1(record.RunnerSessionID)
	preset := c.presets[strings.ToLower(secrets.CreateRequest.SessionParams.OutputProfile)]
	internalToken := InternalMediaTokenV1(c.internalTokenRoot, record.RunnerSessionID)
	inputURL := c.routerRTMPBase + "/" + ingestPath + "?token=" + url.QueryEscape(internalToken)
	outputs := make([]transcode.LiveRTMPOutput, 0, len(preset.Renditions))
	for _, rendition := range preset.Renditions {
		outputPath, _ := RenditionMediaPathV1(record.RunnerSessionID, rendition.Name)
		outputs = append(outputs, transcode.LiveRTMPOutput{Rendition: rendition, URL: c.routerRTMPBase + "/" + outputPath + "?token=" + url.QueryEscape(internalToken), KeyframeInterval: time.Duration(preset.SegmentDuration) * time.Second})
	}
	for {
		if _, err := c.router.WaitForRTMPPublisher(runContext, ingestPath, c.pollInterval); err != nil {
			return
		}
		c.observeGPUPressure(runContext, record.RunnerSessionID)
		process, startErr := c.launcher.Start(runContext, inputURL, outputs, c.hardware, transcode.ProbeResult{})
		exit := LiveLadderExitV1{Code: classifyLiveLadderExitV1(startErr, nil), Err: startErr}
		if startErr == nil {
			c.metrics.RecordLadderStart("started")
			processDone := make(chan LiveLadderExitV1, 1)
			go func() {
				exit := process.Wait()
				c.metrics.RecordLadderExit(exit.Code)
				processDone <- exit
			}()
			select {
			case exit = <-processDone:
			case reason := <-meterDone:
				if reason != "" {
					c.failSession(record, cancel, reason, processDone)
				}
				return
			case <-runContext.Done():
				exit = <-processDone
			}
		} else {
			c.metrics.RecordLadderStart(exit.Code)
		}
		if runContext.Err() != nil {
			return
		}
		publisher, publisherErr := c.router.Path(runContext, ingestPath)
		if publisherErr != nil || !publisher.Online || publisher.Source == nil {
			_ = c.store.RecordIngestPresence(record.BrokerSessionID, false, c.now())
			continue
		}
		attempts, err := c.store.RecordLadderRestart(record.BrokerSessionID, exit.Code, c.now(), c.failureWindow)
		if err != nil {
			return
		}
		c.log.Warn("live ladder exited while publisher remained online", "runner_session_id", record.RunnerSessionID, "code", exit.Code, "attempt", attempts, "diagnostic_tail", exit.DiagnosticTail)
		if attempts >= c.maxFailures {
			if stalled, _ := c.store.RecordOutputStalled(record.BrokerSessionID, c.now()); stalled {
				c.metrics.RecordSessionStalled()
			}
			c.failSession(record, cancel, "output_failed", nil)
			return
		}
		backoff := c.restartInitial
		for count := uint32(1); count < attempts && backoff < c.restartMax; count++ {
			backoff *= 2
		}
		if backoff > c.restartMax {
			backoff = c.restartMax
		}
		timer := time.NewTimer(backoff)
		select {
		case <-runContext.Done():
			timer.Stop()
			return
		case reason := <-meterDone:
			timer.Stop()
			if reason != "" {
				c.failSession(record, cancel, reason, nil)
			}
			return
		case <-timer.C:
		}
	}
}

func (c *LiveRuntimeCoordinatorV1) observeGPUPressure(ctx context.Context, runnerSessionID string) {
	if c.hardware.Vendor != transcode.VendorNVIDIA {
		c.metrics.RecordGPUProbe("unsupported")
		return
	}
	if c.gpuPressure == nil {
		c.metrics.RecordGPUProbe("unavailable")
		c.log.Warn("live ladder GPU pressure unavailable", "runner_session_id", runnerSessionID)
		return
	}
	pressure, err := c.gpuPressure.Sample(ctx)
	if err != nil {
		c.metrics.RecordGPUProbe("unavailable")
		c.log.Warn("live ladder GPU pressure unavailable", "runner_session_id", runnerSessionID)
		return
	}
	c.metrics.RecordGPUProbe("available")
	c.log.Info("live ladder GPU pressure", "runner_session_id", runnerSessionID, "gpu_count", pressure.GPUCount, "encoder_sessions", pressure.EncoderSessions, "memory_used_mib", pressure.MemoryUsedMiB)
}

func (c *LiveRuntimeCoordinatorV1) failSession(record SessionRecordV1, cancel context.CancelFunc, reason string, processDone <-chan LiveLadderExitV1) {
	stopping, _, err := c.store.BeginFailure(record.BrokerSessionID, reason)
	if err != nil || !stopping.Stopping {
		return
	}
	cancel()
	ctx, cleanupCancel := context.WithTimeout(context.Background(), c.cleanupTimeout)
	defer cleanupCancel()
	ingestPath, _ := IngestMediaPathV1(record.RunnerSessionID)
	if err := c.router.KickPublisher(ctx, ingestPath); err != nil {
		c.log.Warn("failed to kick publisher after live output failure", "runner_session_id", record.RunnerSessionID, "error", err)
		return
	}
	if processDone != nil {
		select {
		case <-processDone:
		case <-ctx.Done():
			c.log.Warn("timed out joining failed live ladder", "runner_session_id", record.RunnerSessionID)
			return
		}
	}
	if _, err := FinalizeLiveTerminationV1(c.store, record.BrokerSessionID, c.now()); err != nil {
		c.log.Warn("failed to finalize live output failure", "runner_session_id", record.RunnerSessionID, "error", err)
	}
}

func (c *LiveRuntimeCoordinatorV1) acquireLocal() bool {
	if c.capacity == nil {
		return true
	}
	select {
	case c.capacity <- struct{}{}:
		return true
	default:
		return false
	}
}

func (c *LiveRuntimeCoordinatorV1) releaseLocal() {
	if c.capacity != nil {
		<-c.capacity
	}
}

func (c *LiveRuntimeCoordinatorV1) remove(id string, active *activeLiveSessionV1) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessions[id] == active {
		delete(c.sessions, id)
	}
}
