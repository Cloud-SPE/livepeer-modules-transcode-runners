package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

func testVODRequestV2() VODRequestV2 {
	return VODRequestV2{
		Schema: VODRequestSchemaV2, WorkloadID: "work-1",
		Input:     VODInputV2{DownloadURL: "https://input.invalid/video.mp4"},
		Rendition: VODRenditionV2{Name: "720p", Width: 1280, Height: 720, FPS: 30},
		Output:    VODOutputV2{Stream: VODDestinationV2{ArtifactURI: "cert://720p.mp4", UploadURL: "https://sink.invalid/720p.mp4"}},
	}
}

func TestVODHandlerExecutesOnceAndReplaysTerminalTrailer(t *testing.T) {
	binDir := t.TempDir()
	countPath := filepath.Join(binDir, "ffmpeg-count")
	ffmpeg := filepath.Join(binDir, "ffmpeg")
	ffmpegBody := "#!/bin/sh\nprintf x >> \"$VOD_TEST_COUNT\"\nfor last do :; done\nprintf encoded > \"$last\"\nprintf '%s\\n' 'frame=60' 'out_time=00:00:02.000' 'speed=1.0x' >&2\n"
	if err := os.WriteFile(ffmpeg, []byte(ffmpegBody), 0o700); err != nil {
		t.Fatal(err)
	}
	ffprobe := filepath.Join(binDir, "ffprobe")
	ffprobeBody := "#!/bin/sh\ncase \" $* \" in *' -count_frames '*) printf '%s' '{\"streams\":[{\"nb_read_frames\":\"60\",\"width\":1280,\"height\":720}]}' ;; *) printf '%s' '{\"format\":{\"duration\":\"2\",\"bit_rate\":\"1000\"},\"streams\":[{\"codec_type\":\"video\",\"codec_name\":\"h264\",\"width\":1280,\"height\":720,\"r_frame_rate\":\"30/1\"}]}' ;; esac\n"
	if err := os.WriteFile(ffprobe, []byte(ffprobeBody), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("VOD_TEST_COUNT", countPath)

	input := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "fixture") }))
	defer input.Close()
	var uploads atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method=%s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "encoded" {
			t.Errorf("body=%q", body)
		}
		uploads.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sink.Close()
	service := testVODServiceV2(t)
	req := testVODRequestV2()
	req.Input.DownloadURL = input.URL + "/video.mp4"
	req.Output.Stream.UploadURL = sink.URL + "/720p.mp4"
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	invoke := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/video/transcode", bytes.NewReader(body))
		request.Header.Set("Accept", "text/event-stream")
		service.ServeHTTP(rr, request)
		return rr
	}
	first := invoke()
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "event: result") {
		t.Fatalf("first=%d %s", first.Code, first.Body.String())
	}
	if got := first.Header().Get(VODWorkUnitsTrailerV2); got != "56" {
		t.Fatalf("trailer=%q", got)
	}
	replay := invoke()
	if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), "event: result") {
		t.Fatalf("replay=%d %s", replay.Code, replay.Body.String())
	}
	if got := replay.Header().Get(VODWorkUnitsTrailerV2); got != "56" {
		t.Fatalf("replay trailer=%q", got)
	}
	count, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(count) != "x" {
		t.Fatalf("ffmpeg executions=%q", count)
	}
	if uploads.Load() != 1 {
		t.Fatalf("uploads=%d", uploads.Load())
	}
}

func testVODServiceV2(t *testing.T) *VODServiceV2 {
	t.Helper()
	store, err := NewVODStoreV2(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewVODServiceV2(store, []transcode.Preset{{Name: "h264-720p", VideoCodec: "h264", Width: 1280, Height: 720}}, transcode.HWProfile{}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestVODRequestContractMatchesCertificationShape(t *testing.T) {
	service := testVODServiceV2(t)
	preset, err := service.validateAndResolve(testVODRequestV2())
	if err != nil {
		t.Fatal(err)
	}
	if preset.Name != "h264-720p" || preset.FPS != 30 {
		t.Fatalf("resolved preset = %#v", preset)
	}
	req := testVODRequestV2()
	req.Rendition.Codec = "av1"
	if _, err := service.validateAndResolve(req); err == nil {
		t.Fatal("expected unsupported AV1 shape to fail without an AV1 preset")
	}
}

func TestVODSharedGPUAdmissionRejectsActiveLiveCohort(t *testing.T) {
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
	store, err := NewVODStoreV2(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewVODServiceV2(store, []transcode.Preset{{Name: "h264-720p", VideoCodec: "h264", Width: 1280, Height: 720}}, transcode.HWProfile{}, 4, gate)
	if err != nil {
		t.Fatal(err)
	}
	request := testVODRequestV2()
	hash, err := vodRequestHashV2(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.loadOrCreate(request.WorkloadID, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := service.subscribe(request, service.presets[0], hash, "new"); !errors.Is(err, errVODCapacity) {
		t.Fatalf("subscribe error=%v, want capacity", err)
	}
	if service.gpuAdmissionRejected.Load() != 1 || service.active.Load() != 0 {
		t.Fatalf("rejections=%d active=%d", service.gpuAdmissionRejected.Load(), service.active.Load())
	}
}

func TestVODUsageRoundsAggregateFrameMegapixels(t *testing.T) {
	units, err := vodUnitsV2(&VODVideoV2{ActualFrames: 60, Width: 1280, Height: 720})
	if err != nil {
		t.Fatal(err)
	}
	if units != 56 {
		t.Fatalf("units = %d, want 56", units)
	}
	units, err = vodUnitsV2(nil)
	if err != nil || units != 0 {
		t.Fatalf("audio-only units = %d, err=%v", units, err)
	}
}

func TestVODJournalConvergesConcurrentIdenticalOpens(t *testing.T) {
	store, err := NewVODStoreV2(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const workers = 16
	var wg sync.WaitGroup
	dispositions := make(chan string, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, d, e := store.loadOrCreate("same-workload", strings.Repeat("a", 64))
			dispositions <- d
			errs <- e
		}()
	}
	wg.Wait()
	close(dispositions)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	newCount := 0
	for d := range dispositions {
		if d == "new" {
			newCount++
		} else if d != "resume" {
			t.Fatalf("disposition = %q", d)
		}
	}
	if newCount != 1 {
		t.Fatalf("new dispositions = %d, want 1", newCount)
	}
	if _, d, err := store.loadOrCreate("same-workload", strings.Repeat("b", 64)); err != nil || d != "reuse" {
		t.Fatalf("changed request disposition=%q err=%v", d, err)
	}
}

func TestVODJournalReplaysTerminalClaim(t *testing.T) {
	store, err := NewVODStoreV2(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("c", 64)
	if _, _, err := store.loadOrCreate("terminal", hash); err != nil {
		t.Fatal(err)
	}
	result := VODResultV2{Schema: VODResultSchemaV2, WorkloadID: "terminal", RequestSHA256: hash, Outcome: "succeeded", Usage: VODUsageV2{Unit: VODWorkUnitV2, Units: 56}}
	if err := store.terminal("terminal", "succeeded", "result", result); err != nil {
		t.Fatal(err)
	}
	j, d, err := store.loadOrCreate("terminal", hash)
	if err != nil || d != "terminal" {
		t.Fatalf("disposition=%q err=%v", d, err)
	}
	if got := vodTerminalUnitsV2(j); got != 56 {
		t.Fatalf("terminal units=%d", got)
	}
}

func TestVODRunnerContract(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/livepeer-runner", nil)
	handleRunnerContractV2(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	var contract map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &contract); err != nil {
		t.Fatal(err)
	}
	if contract["capability_id"] != "video:transcode.vod" || contract["protocol"] != "paid-job/v1" {
		t.Fatalf("contract=%v", contract)
	}
	work := contract["work_unit"].(map[string]any)
	extractor := work["extractor"].(map[string]any)
	if extractor["type"] != "response-trailer" || extractor["trailer"] != VODWorkUnitsTrailerV2 {
		t.Fatalf("extractor=%v", extractor)
	}
}
