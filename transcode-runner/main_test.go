package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

func resetRunnerTestState() {
	jobsMu.Lock()
	jobs = make(map[string]*Job)
	jobsMu.Unlock()
	activeJobs.Store(0)
	hw = transcode.HWProfile{}
	presets = []transcode.Preset{
		{
			Name:          "cpu-h264-360p",
			Description:   "CPU test preset",
			VideoCodec:    "h264",
			AudioCodec:    "aac",
			Width:         640,
			Height:        360,
			Bitrate:       "800k",
			MaxRate:       "1200k",
			BufSize:       "2400k",
			PixFmt:        "yuv420p",
			Profile:       "main",
			GPURequired:   false,
			AudioBitrate:  "64k",
			AudioChannels: 2,
		},
	}
	maxQueueSize = 5
}

func TestHandleSubmitRejectsInvalidMethod(t *testing.T) {
	resetRunnerTestState()

	req := httptest.NewRequest(http.MethodGet, "/v1/video/transcode", nil)
	rec := httptest.NewRecorder()

	handleSubmit(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleSubmitRequiresFields(t *testing.T) {
	resetRunnerTestState()

	body := bytes.NewBufferString(`{"output_url":"http://example.com/out.mp4","preset":"cpu-h264-360p"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/video/transcode", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handleSubmit(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if got := rec.Body.String(); !bytes.Contains([]byte(got), []byte("input_url is required")) {
		t.Fatalf("body = %q, want input_url validation error", got)
	}
}

func TestHandleSubmitRejectsUnknownPreset(t *testing.T) {
	resetRunnerTestState()

	body := bytes.NewBufferString(`{"input_url":"http://example.com/in.mp4","output_url":"http://example.com/out.mp4","preset":"missing"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/video/transcode", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handleSubmit(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if got := rec.Body.String(); !bytes.Contains([]byte(got), []byte("unknown preset")) {
		t.Fatalf("body = %q, want unknown preset error", got)
	}
}

func TestHandleStatusReturnsJob(t *testing.T) {
	resetRunnerTestState()

	job := &Job{
		ID:        "job-123",
		Status:    "complete",
		Phase:     "complete",
		Progress:  100,
		CreatedAt: time.Unix(1700000000, 0),
		StartedAt: time.Unix(1700000000, 0),
	}

	jobsMu.Lock()
	jobs[job.ID] = job
	jobsMu.Unlock()

	body := bytes.NewBufferString(`{"job_id":"job-123"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/video/transcode/status", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handleStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp JobStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.JobID != job.ID {
		t.Fatalf("job_id = %q, want %q", resp.JobID, job.ID)
	}
	if resp.Status != "complete" {
		t.Fatalf("status = %q, want complete", resp.Status)
	}
}

func TestHandleStatusReturnsNotFound(t *testing.T) {
	resetRunnerTestState()

	body := bytes.NewBufferString(`{"job_id":"missing"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/video/transcode/status", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handleStatus(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleHealthzReportsState(t *testing.T) {
	resetRunnerTestState()
	hw = transcode.HWProfile{GPUName: "Test GPU", VRAM_MB: 16384}
	activeJobs.Store(2)
	maxQueueSize = 7

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	handleHealthz(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["gpu"] != "Test GPU" {
		t.Fatalf("gpu = %v, want Test GPU", resp["gpu"])
	}
	if got := int(resp["active_jobs"].(float64)); got != 2 {
		t.Fatalf("active_jobs = %d, want 2", got)
	}
	if got := int(resp["presets"].(float64)); got != 1 {
		t.Fatalf("presets = %d, want 1", got)
	}
}
