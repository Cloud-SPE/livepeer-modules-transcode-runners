package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

type fakeABRExecutorV2 struct {
	calls             atomic.Int32
	started           chan struct{}
	release           chan struct{}
	canceled          chan struct{}
	fail              error
	failAfterDelivery error
	once              sync.Once
}

func (f *fakeABRExecutorV2) Execute(ctx context.Context, req ABRWorkloadRequestV2, _ transcode.ABRPreset, reporter ABRExecutionReporterV2) (ABRTerminalResultV2, error) {
	f.calls.Add(1)
	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			if f.canceled != nil {
				close(f.canceled)
			}
			return ABRTerminalResultV2{}, ctx.Err()
		}
	}
	if f.fail != nil {
		return ABRTerminalResultV2{}, f.fail
	}
	if err := reporter.Progress("encoding", 50, "720p", 100); err != nil {
		return ABRTerminalResultV2{}, err
	}
	prepared := PreparedRenditionV2{
		Name: "720p", PlaylistPath: "work/asset/720p/playlist.m3u8", StreamPath: "work/asset/720p/stream.mp4",
		PlaylistSHA256: strings.Repeat("a", 64), StreamSHA256: strings.Repeat("b", 64),
		Video: &DeliveredVideoV2{ActualFrames: 300, Width: 1280, Height: 720}, FileSizeBytes: 42,
	}
	if err := reporter.Prepared(prepared); err != nil {
		return ABRTerminalResultV2{}, err
	}
	delivered := RenditionResultV2{
		Name: "720p", PlaylistURI: req.Output.Renditions["720p"].Playlist.ArtifactURI,
		StreamURI: req.Output.Renditions["720p"].Stream.ArtifactURI, Video: prepared.Video, FileSizeBytes: 42,
	}
	if err := reporter.Delivered(delivered); err != nil {
		return ABRTerminalResultV2{}, err
	}
	if f.failAfterDelivery != nil {
		return ABRTerminalResultV2{}, f.failAfterDelivery
	}
	if err := reporter.PreparedManifest(PreparedArtifactV2{Path: "work/asset/master.m3u8", SHA256: strings.Repeat("c", 64)}); err != nil {
		return ABRTerminalResultV2{}, err
	}
	if err := reporter.DeliveredManifest(req.Output.Manifest.ArtifactURI); err != nil {
		return ABRTerminalResultV2{}, err
	}
	hash, _ := RequestContentSHA256V2(req)
	units, _ := CalculateFrameMegapixelUnitsV2([]RenditionResultV2{delivered})
	return ABRTerminalResultV2{
		Schema: ABRResultSchemaV2, WorkloadID: req.WorkloadID, RequestSHA256: hash, Outcome: "succeeded",
		ManifestURI: req.Output.Manifest.ArtifactURI, Renditions: []RenditionResultV2{delivered},
		Usage: UsageClaimV2{Unit: ABRWorkUnitV2, Units: units},
	}, nil
}

func TestABRHandlerV2KeepsExchangeOpenThroughTerminalSuccess(t *testing.T) {
	handler, executor, store := newTestABRHandlerV2(t, &fakeABRExecutorV2{})
	recorder := performABRRequestV2(t, handler, testABRRequestV2("asset-success"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "event: progress") || !strings.Contains(body, "event: result") || strings.Contains(body, "upload_url") || strings.Contains(body, "sig=") {
		t.Fatalf("unsafe or incomplete SSE response: %s", body)
	}
	if executor.calls.Load() != 1 {
		t.Fatalf("executor calls = %d", executor.calls.Load())
	}
	if got := recorder.Result().Trailer.Get(ABRWorkUnitsTrailerV2); got != "277" {
		t.Fatalf("work-unit trailer = %q, want 277", got)
	}
	record, err := store.Load("asset-success")
	if err != nil || record.State != WorkloadSucceededV2 {
		t.Fatalf("terminal journal = %q, %v", record.State, err)
	}
}

func TestABRHandlerV2FailureIsTerminalZeroUsageAndRedacted(t *testing.T) {
	executor := &fakeABRExecutorV2{fail: &ABRExecutionErrorV2{Code: "download_failed", Message: "input download failed", Retryable: true}}
	handler, _, store := newTestABRHandlerV2(t, executor)
	recorder := performABRRequestV2(t, handler, testABRRequestV2("asset-failure"))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "event: error") || !strings.Contains(body, `"units":0`) {
		t.Fatalf("failure SSE status=%d body=%s", recorder.Code, body)
	}
	if strings.Contains(body, "storage.example") || strings.Contains(body, "sig=") {
		t.Fatalf("failure leaked credentials: %s", body)
	}
	if got := recorder.Result().Trailer.Get(ABRWorkUnitsTrailerV2); got != "0" {
		t.Fatalf("failed work-unit trailer = %q, want 0", got)
	}
	record, err := store.Load("asset-failure")
	if err != nil || record.State != WorkloadFailedV2 {
		t.Fatalf("failure journal = %q, %v", record.State, err)
	}
}

func TestABRHandlerV2PartialFailureBillsDeliveredRenditions(t *testing.T) {
	executor := &fakeABRExecutorV2{failAfterDelivery: &ABRExecutionErrorV2{Code: "upload_failed", Message: "later rendition upload failed", Retryable: true}}
	handler, _, store := newTestABRHandlerV2(t, executor)
	recorder := performABRRequestV2(t, handler, testABRRequestV2("asset-partial"))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "event: error") || !strings.Contains(body, `"units":277`) {
		t.Fatalf("partial failure SSE status=%d body=%s", recorder.Code, body)
	}
	if got := recorder.Result().Trailer.Get(ABRWorkUnitsTrailerV2); got != "277" {
		t.Fatalf("partial failure trailer = %q, want 277", got)
	}
	record, err := store.Load("asset-partial")
	if err != nil || record.State != WorkloadFailedV2 || len(record.Delivered) != 1 {
		t.Fatalf("partial failure journal = state %q delivered %d err=%v", record.State, len(record.Delivered), err)
	}
}

func TestABRHandlerV2ConcurrentReplayAttachesWithoutDuplicateExecution(t *testing.T) {
	executor := &fakeABRExecutorV2{started: make(chan struct{}), release: make(chan struct{})}
	handler, _, _ := newTestABRHandlerV2(t, executor)
	req := testABRRequestV2("asset-concurrent")

	type response struct{ recorder *httptest.ResponseRecorder }
	responses := make(chan response, 2)
	run := func() { responses <- response{performABRRequestV2(t, handler, req)} }
	go run()
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("executor did not start")
	}
	go run()
	time.Sleep(20 * time.Millisecond)
	if executor.calls.Load() != 1 {
		t.Fatalf("concurrent replay launched %d executions", executor.calls.Load())
	}
	close(executor.release)
	for i := 0; i < 2; i++ {
		select {
		case response := <-responses:
			if response.recorder.Code != http.StatusOK || !strings.Contains(response.recorder.Body.String(), "event: result") {
				t.Fatalf("attached response = %d %s", response.recorder.Code, response.recorder.Body.String())
			}
		case <-time.After(time.Second):
			t.Fatal("attached response did not terminate")
		}
	}
	if executor.calls.Load() != 1 {
		t.Fatalf("executor calls = %d", executor.calls.Load())
	}
}

func TestABRHandlerV2TerminalReplayAndContentMismatch(t *testing.T) {
	handler, executor, _ := newTestABRHandlerV2(t, &fakeABRExecutorV2{})
	req := testABRRequestV2("asset-replay")
	first := performABRRequestV2(t, handler, req)
	second := performABRRequestV2(t, handler, req)
	withoutKeepalive := func(value string) string { return strings.ReplaceAll(value, ": keepalive\n\n", "") }
	if first.Code != http.StatusOK || second.Code != http.StatusOK || withoutKeepalive(first.Body.String()) != withoutKeepalive(second.Body.String()) {
		t.Fatalf("terminal replay differs:\nfirst=%s\nsecond=%s", first.Body.String(), second.Body.String())
	}
	if first.Result().Trailer.Get(ABRWorkUnitsTrailerV2) != "277" || second.Result().Trailer.Get(ABRWorkUnitsTrailerV2) != "277" {
		t.Fatalf("terminal replay trailers = %q, %q", first.Result().Trailer.Get(ABRWorkUnitsTrailerV2), second.Result().Trailer.Get(ABRWorkUnitsTrailerV2))
	}

	req.Input.DownloadURL = "https://storage.example/different.mp4?sig=different"
	mismatch := performABRRequestV2(t, handler, req)
	if mismatch.Code != http.StatusConflict || !strings.Contains(mismatch.Body.String(), "workload_id_reuse") {
		t.Fatalf("mismatch = %d %s", mismatch.Code, mismatch.Body.String())
	}
	if executor.calls.Load() != 1 {
		t.Fatalf("replay launched %d executions", executor.calls.Load())
	}
}

func TestABRHandlerV2DisconnectCancelsAndLeavesWorkRecoverable(t *testing.T) {
	executor := &fakeABRExecutorV2{started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{})}
	handler, _, store := newTestABRHandlerV2(t, executor)
	req := testABRRequestV2("asset-cancel")
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/video/transcode/abr", bytes.NewReader(body)).WithContext(ctx)
	httpReq.Header.Set("Accept", "text/event-stream")
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), httpReq)
		close(done)
	}()
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("executor did not start")
	}
	cancel()
	select {
	case <-executor.canceled:
	case <-time.After(time.Second):
		t.Fatal("executor was not canceled after disconnect")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not return after disconnect")
	}
	record, err := store.Load(req.WorkloadID)
	if err != nil || record.State != WorkloadInProgressV2 || record.TerminalEvent != nil {
		t.Fatalf("canceled journal = %q terminal=%v err=%v", record.State, record.TerminalEvent != nil, err)
	}
}

func TestABRHandlerV2RejectsNonSSEAndUnknownJSON(t *testing.T) {
	handler, _, _ := newTestABRHandlerV2(t, &fakeABRExecutorV2{})
	req := testABRRequestV2("asset-invalid")
	body, _ := json.Marshal(req)
	request := httptest.NewRequest(http.MethodPost, "/v1/video/transcode/abr", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotAcceptable {
		t.Fatalf("non-SSE status = %d", recorder.Code)
	}

	body = bytes.Replace(body, []byte(`"schema":`), []byte(`"unknown":true,"schema":`), 1)
	request = httptest.NewRequest(http.MethodPost, "/v1/video/transcode/abr", bytes.NewReader(body))
	request.Header.Set("Accept", "text/event-stream")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "unknown field") {
		t.Fatalf("unknown JSON status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func newTestABRHandlerV2(t *testing.T, executor *fakeABRExecutorV2) (*ABRHandlerV2, *fakeABRExecutorV2, *FileWorkloadStoreV2) {
	t.Helper()
	store, err := NewFileWorkloadStoreV2(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewABRExecutionCoordinatorV2(store, executor, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewABRHandlerV2(store, coordinator, []transcode.ABRPreset{testABRPresetV2()})
	if err != nil {
		t.Fatal(err)
	}
	handler.keepalive = 10 * time.Millisecond
	return handler, executor, store
}

func TestABRSharedGPUAdmissionRejectsActiveLiveCohort(t *testing.T) {
	directory := t.TempDir()
	gate, err := transcode.NewGPUAdmissionGate(filepath.Join(directory, "gpu.lock"))
	if err != nil {
		t.Fatal(err)
	}
	liveLease, err := gate.Acquire(transcode.GPUAdmissionLive)
	if err != nil {
		t.Fatal(err)
	}
	defer liveLease.Close()
	store, err := NewFileWorkloadStoreV2(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executor := &fakeABRExecutorV2{}
	coordinator, err := NewABRExecutionCoordinatorV2(store, executor, 4, gate)
	if err != nil {
		t.Fatal(err)
	}
	request := testABRRequestV2("gpu-admission")
	hash, err := RequestContentSHA256V2(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LoadOrCreate(request.WorkloadID, hash); err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Subscribe(request, hash, testABRPresetV2()); !errors.Is(err, errABRExecutionCapacityV2) {
		t.Fatalf("subscribe error=%v, want capacity", err)
	}
	if coordinator.gpuAdmissionRejected.Load() != 1 || coordinator.Active() != 0 {
		t.Fatalf("rejections=%d active=%d", coordinator.gpuAdmissionRejected.Load(), coordinator.Active())
	}
}

func performABRRequestV2(t *testing.T, handler http.Handler, req ABRWorkloadRequestV2) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/video/transcode/abr", bytes.NewReader(body))
	httpReq.Header.Set("Accept", "text/event-stream")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httpReq)
	return recorder
}

func testABRPresetV2() transcode.ABRPreset {
	return transcode.ABRPreset{
		Name: "test", SegmentDuration: 6,
		Renditions: []transcode.ABRRendition{{
			Name: "720p", Video: &transcode.ABRVideoSettings{Codec: "h264", Width: 1280, Height: 720},
		}},
	}
}

func testABRRequestV2(workloadID string) ABRWorkloadRequestV2 {
	return ABRWorkloadRequestV2{
		Schema: ABRRequestSchemaV2, WorkloadID: workloadID,
		Input:  ABRInputV2{DownloadURL: "https://storage.example/input.mp4?sig=input"},
		Ladder: ABRLadderV2{Preset: "test"},
		Output: ABROutputDestinationsV2{
			Manifest: ArtifactDestinationV2{ArtifactURI: "vod/asset/master.m3u8", UploadURL: "https://storage.example/master?sig=master"},
			Renditions: map[string]RenditionDestinationsV2{
				"720p": {
					Playlist: ArtifactDestinationV2{ArtifactURI: "vod/asset/720p/playlist.m3u8", UploadURL: "https://storage.example/playlist?sig=playlist"},
					Stream:   ArtifactDestinationV2{ArtifactURI: "vod/asset/720p/stream.mp4", UploadURL: "https://storage.example/stream?sig=stream"},
				},
			},
		},
	}
}
