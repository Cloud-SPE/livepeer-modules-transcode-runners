package liverunner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

type fakePublisherWaiterV1 struct {
	ready   <-chan struct{}
	kicked  chan<- string
	kickErr error
}

func (f fakePublisherWaiterV1) KickPublisher(_ context.Context, path string) error {
	if f.kicked != nil {
		f.kicked <- path
	}
	return f.kickErr
}

func (f fakePublisherWaiterV1) Path(_ context.Context, path string) (MediaPathStatusV1, error) {
	select {
	case <-f.ready:
		return MediaPathStatusV1{Name: path, Online: true, Source: &MediaPathSourceV1{Type: "rtmpConn"}}, nil
	default:
		return MediaPathStatusV1{}, ErrMediaPathNotFoundV1
	}
}

func (f fakePublisherWaiterV1) WaitForRTMPPublisher(ctx context.Context, path string, _ time.Duration) (MediaPathStatusV1, error) {
	select {
	case <-ctx.Done():
		return MediaPathStatusV1{}, ctx.Err()
	case <-f.ready:
		return MediaPathStatusV1{Name: path, Online: true, Source: &MediaPathSourceV1{Type: "rtmpConn"}}, nil
	}
}

type fakeLiveProcessV1 struct {
	ctx context.Context
}

type failedLiveProcessV1 struct{ exit LiveLadderExitV1 }

func (p failedLiveProcessV1) Wait() LiveLadderExitV1 { return p.exit }

type failedLiveLauncherV1 struct{ code string }

type fakeGPUPressureSamplerV1 struct {
	pressure GPUPressureV1
	err      error
}

func (f fakeGPUPressureSamplerV1) Sample(context.Context) (GPUPressureV1, error) {
	return f.pressure, f.err
}

func (f failedLiveLauncherV1) Start(context.Context, string, []transcode.LiveRTMPOutput, transcode.HWProfile, transcode.ProbeResult) (LiveLadderProcessV1, error) {
	return failedLiveProcessV1{exit: LiveLadderExitV1{Code: f.code, Err: errors.New("safe test failure"), DiagnosticTail: []string{"safe diagnostic"}}}, nil
}

type noopLiveMeterV1 struct{}

func (noopLiveMeterV1) Run(ctx context.Context, _ SessionRecordV1, _ SessionSecretsV1) string {
	<-ctx.Done()
	return ""
}

type trackingLiveMeterV1 struct {
	started chan struct{}
	stopped chan struct{}
}

func (m trackingLiveMeterV1) Run(ctx context.Context, _ SessionRecordV1, _ SessionSecretsV1) string {
	close(m.started)
	<-ctx.Done()
	close(m.stopped)
	return ""
}

func (p fakeLiveProcessV1) Wait() LiveLadderExitV1 {
	<-p.ctx.Done()
	return LiveLadderExitV1{Code: "process_killed", Err: p.ctx.Err()}
}

type liveLaunchV1 struct {
	input   string
	outputs []transcode.LiveRTMPOutput
}

type fakeLiveLauncherV1 struct {
	mu       sync.Mutex
	launches []liveLaunchV1
	started  chan struct{}
}

func (f *fakeLiveLauncherV1) Start(ctx context.Context, input string, outputs []transcode.LiveRTMPOutput, _ transcode.HWProfile, _ transcode.ProbeResult) (LiveLadderProcessV1, error) {
	f.mu.Lock()
	f.launches = append(f.launches, liveLaunchV1{input: input, outputs: append([]transcode.LiveRTMPOutput(nil), outputs...)})
	f.mu.Unlock()
	select {
	case f.started <- struct{}{}:
	default:
	}
	return fakeLiveProcessV1{ctx: ctx}, nil
}

func TestLiveRuntimeWaitsForPublisherLaunchesOnceAndTerminates(t *testing.T) {
	ready := make(chan struct{})
	kicked := make(chan string, 1)
	launcher := &fakeLiveLauncherV1{started: make(chan struct{}, 2)}
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x72}, 32))
	coordinator := newTestRuntimeCoordinatorV1(t, store, fakePublisherWaiterV1{ready: ready, kicked: kicked}, launcher, 1)
	record, secrets := createRuntimeSessionV1(t, store, "sess_runtime_001", "runner_runtime_001")
	if err := coordinator.EnsureSession(context.Background(), record, secrets); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.EnsureSession(context.Background(), record, secrets); err != nil {
		t.Fatal(err)
	}
	select {
	case <-launcher.started:
		t.Fatal("FFmpeg started before an RTMP publisher was online")
	default:
	}
	close(ready)
	select {
	case <-launcher.started:
	case <-time.After(time.Second):
		t.Fatal("FFmpeg did not start after publisher readiness")
	}
	launcher.mu.Lock()
	if len(launcher.launches) != 1 {
		t.Fatalf("launch count=%d", len(launcher.launches))
	}
	launch := launcher.launches[0]
	launcher.mu.Unlock()
	if !strings.HasPrefix(launch.input, "rtmp://127.0.0.1:1935/ingest/runner_runtime_001?token=") || len(launch.outputs) != 2 {
		t.Fatalf("launch input=%q outputs=%+v", launch.input, launch.outputs)
	}
	for _, output := range launch.outputs {
		if !strings.Contains(output.URL, "/renditions/runner_runtime_001/"+output.Rendition.Name+"?token=") {
			t.Fatalf("rendition URL=%q", output.URL)
		}
	}
	persisted, _, err := store.Load(record.BrokerSessionID)
	if err != nil || persisted.LastSequence != 1 || len(persisted.PendingEvents) != 1 || persisted.PendingEvents[0].EventType != "session.started" {
		t.Fatalf("started event=%+v err=%v", persisted, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := coordinator.TerminateSession(ctx, record); err != nil {
		t.Fatal(err)
	}
	select {
	case path := <-kicked:
		if path != "ingest/runner_runtime_001" {
			t.Fatalf("terminated ingest path=%q", path)
		}
	default:
		t.Fatal("termination did not kick the ingest publisher")
	}
}

func TestLiveGPUAdmissionRejectsBatchLeaseAndRecovers(t *testing.T) {
	directory := t.TempDir()
	gate, err := transcode.NewGPUAdmissionGate(filepath.Join(directory, "gpu.lock"))
	if err != nil {
		t.Fatal(err)
	}
	batchLease, err := gate.Acquire(transcode.GPUAdmissionBatch)
	if err != nil {
		t.Fatal(err)
	}
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x71}, 32))
	coordinator, err := NewLiveRuntimeCoordinatorV1(store, fakePublisherWaiterV1{ready: make(chan struct{})}, &fakeLiveLauncherV1{}, noopLiveMeterV1{}, testLivePresetsV1(), transcode.HWProfile{}, "rtmp://127.0.0.1:1935", strings.Repeat("i", 32), time.Millisecond, 4, gate)
	if err != nil {
		t.Fatal(err)
	}
	record, secrets := createRuntimeSessionV1(t, store, "sess_gpu_admission", "runner_gpu_admission")
	if err := coordinator.EnsureSession(context.Background(), record, secrets); !errors.Is(err, transcode.ErrGPUAdmissionCapacity) {
		t.Fatalf("ensure error=%v, want capacity", err)
	}
	if coordinator.metrics.gpuAdmissionRejected.Load() != 1 || len(coordinator.sessions) != 0 {
		t.Fatalf("rejections=%d sessions=%d", coordinator.metrics.gpuAdmissionRejected.Load(), len(coordinator.sessions))
	}
	if err := batchLease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.EnsureSession(context.Background(), record, secrets); err != nil {
		t.Fatalf("ensure after batch release: %v", err)
	}
	secondRecord, secondSecrets := createRuntimeSessionV1(t, store, "sess_gpu_admission_two", "runner_gpu_admission_two")
	if err := coordinator.EnsureSession(context.Background(), secondRecord, secondSecrets); err != nil {
		t.Fatalf("second live session in same cohort: %v", err)
	}
	if len(coordinator.sessions) != 2 {
		t.Fatalf("same-cohort live sessions=%d, want 2", len(coordinator.sessions))
	}
	shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := coordinator.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	probe, err := gate.Acquire(transcode.GPUAdmissionBatch)
	if err != nil {
		t.Fatalf("live shutdown did not release exclusive lease: %v", err)
	}
	defer probe.Close()
}

func TestLiveRuntimeTerminationWithoutActiveFFmpegPropagatesKickFailure(t *testing.T) {
	kicked := make(chan string, 1)
	wantErr := errors.New("router unavailable")
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x76}, 32))
	router := fakePublisherWaiterV1{ready: make(chan struct{}), kicked: kicked, kickErr: wantErr}
	coordinator := newTestRuntimeCoordinatorV1(t, store, router, &fakeLiveLauncherV1{}, 1)
	record, _ := createRuntimeSessionV1(t, store, "sess_runtime_003", "runner_runtime_003")
	if err := coordinator.TerminateSession(context.Background(), record); !errors.Is(err, wantErr) {
		t.Fatalf("termination error=%v", err)
	}
	select {
	case path := <-kicked:
		if path != "ingest/runner_runtime_003" {
			t.Fatalf("terminated ingest path=%q", path)
		}
	default:
		t.Fatal("termination did not attempt to kick the ingest publisher")
	}
}

func TestLiveRuntimeTerminationJoinsSessionMeter(t *testing.T) {
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x77}, 32))
	meter := trackingLiveMeterV1{started: make(chan struct{}), stopped: make(chan struct{})}
	coordinator, err := NewLiveRuntimeCoordinatorV1(store, fakePublisherWaiterV1{ready: make(chan struct{})}, &fakeLiveLauncherV1{}, meter, testLivePresetsV1(), transcode.HWProfile{}, "rtmp://127.0.0.1:1935", strings.Repeat("i", 32), time.Millisecond, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, secrets := createRuntimeSessionV1(t, store, "sess_runtime_meter", "runner_runtime_meter")
	if err := coordinator.EnsureSession(context.Background(), record, secrets); err != nil {
		t.Fatal(err)
	}
	select {
	case <-meter.started:
	case <-time.After(time.Second):
		t.Fatal("session meter did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := coordinator.TerminateSession(ctx, record); err != nil {
		t.Fatal(err)
	}
	select {
	case <-meter.stopped:
	default:
		t.Fatal("termination returned before the session meter stopped")
	}
}

func TestLiveRuntimeRestartDoesNotDuplicateStartedEvent(t *testing.T) {
	ready := make(chan struct{})
	launcher := &fakeLiveLauncherV1{started: make(chan struct{}, 4)}
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x73}, 32))
	first := newTestRuntimeCoordinatorV1(t, store, fakePublisherWaiterV1{ready: ready}, launcher, 1)
	record, secrets := createRuntimeSessionV1(t, store, "sess_runtime_002", "runner_runtime_002")
	if err := first.EnsureSession(context.Background(), record, secrets); err != nil {
		t.Fatal(err)
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
	if err := first.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
	cancel()
	persisted, persistedSecrets, err := store.Load(record.BrokerSessionID)
	if err != nil || persistedSecrets == nil {
		t.Fatal(err)
	}
	second := newTestRuntimeCoordinatorV1(t, store, fakePublisherWaiterV1{ready: ready}, launcher, 1)
	if err := second.EnsureSession(context.Background(), persisted, *persistedSecrets); err != nil {
		t.Fatal(err)
	}
	defer second.Shutdown(context.Background())
	persisted, _, err = store.Load(record.BrokerSessionID)
	if err != nil || persisted.LastSequence != 1 || len(persisted.PendingEvents) != 1 {
		t.Fatalf("restart duplicated started event: sequence=%d pending=%d err=%v", persisted.LastSequence, len(persisted.PendingEvents), err)
	}
}

func TestLiveRuntimeValidatesProfileAndMeteringRendition(t *testing.T) {
	ready := make(chan struct{})
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x74}, 32))
	coordinator := newTestRuntimeCoordinatorV1(t, store, fakePublisherWaiterV1{ready: ready}, &fakeLiveLauncherV1{started: make(chan struct{}, 1)}, 1)
	request := readStrictFixtureV1[RunnerCreateRequestV1](t, "create-request.json")
	request.SessionParams.OutputProfile = "unknown"
	if err := coordinator.ValidateSession(request); err == nil {
		t.Fatal("unknown live profile was accepted")
	}
	request.SessionParams.OutputProfile = "live-standard"
	request.SessionParams.MeteringRendition = "missing"
	if err := coordinator.ValidateSession(request); err == nil {
		t.Fatal("missing metering rendition was accepted")
	}
	request.SessionParams.MeteringRendition = "720p"
	if err := coordinator.ValidateSession(request); err != nil {
		t.Fatalf("valid live profile rejected: %v", err)
	}
}

func TestLiveRuntimeConstructorRejectsNonRTMPRouter(t *testing.T) {
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x75}, 32))
	_, err := NewLiveRuntimeCoordinatorV1(store, fakePublisherWaiterV1{ready: make(chan struct{})}, &fakeLiveLauncherV1{}, noopLiveMeterV1{}, testLivePresetsV1(), transcode.HWProfile{}, "http://127.0.0.1:1935", strings.Repeat("x", 32), time.Millisecond, 1, nil)
	if err == nil {
		t.Fatal("non-RTMP internal router URL was accepted")
	}
}

func TestLiveRuntimeBoundsRestartsAndFailsOutput(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	kicked := make(chan string, 2)
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x78}, 32))
	coordinator, err := NewLiveRuntimeCoordinatorV1(store, fakePublisherWaiterV1{ready: ready, kicked: kicked}, failedLiveLauncherV1{code: "encoder_init_failed"}, noopLiveMeterV1{}, testLivePresetsV1(), transcode.HWProfile{}, "rtmp://127.0.0.1:1935", strings.Repeat("i", 32), time.Millisecond, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.restartInitial = time.Millisecond
	coordinator.restartMax = time.Millisecond
	coordinator.maxFailures = 2
	record, secrets := createRuntimeSessionV1(t, store, "sess_runtime_failed", "runner_runtime_failed")
	if err := coordinator.EnsureSession(context.Background(), record, secrets); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		persisted, _, loadErr := store.Load(record.BrokerSessionID)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if persisted.State == "failed" {
			if persisted.CloseReason != "output_failed" || persisted.OutputState != OutputStateStalledV1 || len(persisted.PendingEvents) != 5 {
				t.Fatalf("failed session=%+v", persisted)
			}
			if persisted.PendingEvents[1].EventType != "session.ladder.restart" || persisted.PendingEvents[2].EventType != "session.ladder.restart" || persisted.PendingEvents[3].EventType != "session.output.stalled" || persisted.PendingEvents[4].EventType != "session.failed" {
				t.Fatalf("failure events=%+v", persisted.PendingEvents)
			}
			codeIndex := ladderMetricCodeIndexV1("encoder_init_failed")
			startedIndex := ladderMetricCodeIndexV1("started")
			if coordinator.metrics.ladderStarts[startedIndex].Load() != 2 || coordinator.metrics.ladderExits[codeIndex].Load() != 2 || coordinator.metrics.sessionsStalled.Load() != 1 || coordinator.metrics.gpuProbes[2].Load() != 2 {
				t.Fatalf("runtime metrics starts=%d exits=%d stalled=%d unsupported=%d", coordinator.metrics.ladderStarts[startedIndex].Load(), coordinator.metrics.ladderExits[codeIndex].Load(), coordinator.metrics.sessionsStalled.Load(), coordinator.metrics.gpuProbes[2].Load())
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("runtime did not fail after bounded ladder restarts")
}

func TestLiveLadderDiagnosticsAreSanitizedAndClassified(t *testing.T) {
	unsafe := "Error opening input rtmp://runner.example/ingest/id?token=super-secret"
	safe := sanitizeFFmpegDiagnosticV1(unsafe)
	if strings.Contains(safe, "super-secret") || !strings.Contains(safe, "rtmp://[redacted]") {
		t.Fatalf("unsafe diagnostic=%q", safe)
	}
	tests := map[string]string{
		"Error while opening encoder - maybe incorrect parameters": "encoder_init_failed",
		"Cannot load libcuda.so.1":                                 "hwaccel_init_failed",
		"Error opening input: Connection refused":                  "input_unavailable",
		"Error writing trailer: Broken pipe":                       "output_rejected",
		"unrecognized failure":                                     "unknown",
	}
	for diagnostic, want := range tests {
		if got := classifyLiveLadderExitV1(errors.New("exit status 1"), []string{diagnostic}); got != want {
			t.Fatalf("diagnostic=%q code=%q want=%q", diagnostic, got, want)
		}
	}
}

func TestLiveRuntimeLogsSafeNVIDIAPressureAndDegradesTelemetryFailure(t *testing.T) {
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x72}, 32))
	coordinator := newTestRuntimeCoordinatorV1(t, store, fakePublisherWaiterV1{ready: make(chan struct{})}, &fakeLiveLauncherV1{}, 1)
	coordinator.hardware = transcode.HWProfile{Vendor: transcode.VendorNVIDIA}
	coordinator.metrics = &LiveRunnerMetricsV1{}
	var logs bytes.Buffer
	coordinator.log = slog.New(slog.NewTextHandler(&logs, nil))
	coordinator.gpuPressure = fakeGPUPressureSamplerV1{pressure: GPUPressureV1{GPUCount: 1, EncoderSessions: 4, MemoryUsedMiB: 8192}}
	coordinator.observeGPUPressure(context.Background(), "runner_gpu_metrics")
	if text := logs.String(); !strings.Contains(text, "encoder_sessions=4") || !strings.Contains(text, "memory_used_mib=8192") || coordinator.metrics.gpuProbes[0].Load() != 1 {
		t.Fatalf("available GPU telemetry log=%q metric=%d", text, coordinator.metrics.gpuProbes[0].Load())
	}
	logs.Reset()
	coordinator.gpuPressure = fakeGPUPressureSamplerV1{err: errors.New("token=unsafe-driver-detail")}
	coordinator.observeGPUPressure(context.Background(), "runner_gpu_metrics")
	if text := logs.String(); strings.Contains(text, "unsafe-driver-detail") || !strings.Contains(text, "pressure unavailable") || coordinator.metrics.gpuProbes[1].Load() != 1 {
		t.Fatalf("unavailable GPU telemetry log=%q metric=%d", text, coordinator.metrics.gpuProbes[1].Load())
	}
}

func newTestRuntimeCoordinatorV1(t *testing.T, store *EncryptedFileSessionStoreV1, router MediaPublisherWaiterV1, launcher LiveLadderLauncherV1, capacity int) *LiveRuntimeCoordinatorV1 {
	t.Helper()
	coordinator, err := NewLiveRuntimeCoordinatorV1(store, router, launcher, noopLiveMeterV1{}, testLivePresetsV1(), transcode.HWProfile{}, "rtmp://127.0.0.1:1935", strings.Repeat("i", 32), time.Millisecond, capacity, nil)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func createRuntimeSessionV1(t *testing.T, store *EncryptedFileSessionStoreV1, brokerSessionID, runnerSessionID string) (SessionRecordV1, SessionSecretsV1) {
	t.Helper()
	request, response := testCreatePairV1(t)
	request.SessionID = brokerSessionID
	request.WorkID = "loc-auth:" + brokerSessionID
	request.SessionParams.OutputProfile = "live-standard"
	response.RunnerSessionID = runnerSessionID
	response.Runtime.Public.HLSURL = "https://runner.example/hls/" + runnerSessionID + "/master.m3u8"
	response.Runtime.Public.KeyIssueURL = "https://runner.example/v1/sessions/" + runnerSessionID + "/stream-keys"
	response.Runtime.Public.StatusURL = "https://runner.example/v1/public/sessions/" + runnerSessionID + "/status"
	record, secrets, _, err := store.CreateOrReplay(request, response)
	if err != nil {
		t.Fatal(err)
	}
	return record, secrets
}

func testLivePresetsV1() []transcode.ABRPreset {
	return []transcode.ABRPreset{{
		Name: "live-standard", Renditions: []transcode.ABRRendition{
			{Name: "720p", Video: &transcode.ABRVideoSettings{Codec: "h264", Width: 1280, Height: 720, Bitrate: "2.5M", MaxBitrate: "3.75M"}, Audio: transcode.ABRAudioSettings{Codec: "aac", Bitrate: "96k", Channels: 2}},
			{Name: "360p", Video: &transcode.ABRVideoSettings{Codec: "h264", Width: 640, Height: 360, Bitrate: "600k", MaxBitrate: "900k"}, Audio: transcode.ABRAudioSettings{Codec: "aac", Bitrate: "64k", Channels: 2}},
		}, SegmentDuration: 1,
	}}
}
