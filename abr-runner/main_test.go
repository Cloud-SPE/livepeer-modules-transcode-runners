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

func resetABRTestState() {
	jobsMu.Lock()
	jobs = make(map[string]*ABRJob)
	jobsMu.Unlock()
	activeJobs.Store(0)
	hw = transcode.HWProfile{}
	abrPresets = []transcode.ABRPreset{
		{
			Name:            "abr-test",
			Description:     "ABR test preset",
			Type:            "abr",
			Format:          "hls",
			HLSMode:         "fmp4_single_file",
			SegmentDuration: 6,
			Renditions: []transcode.ABRRendition{
				{
					Name: "360p",
					Video: &transcode.ABRVideoSettings{
						Codec:      "h264",
						Width:      640,
						Height:     360,
						Bitrate:    "800k",
						MaxBitrate: "1200k",
						PixFmt:     "yuv420p",
					},
					Audio: transcode.ABRAudioSettings{
						Codec:    "aac",
						Bitrate:  "64k",
						Channels: 2,
					},
				},
			},
		},
	}
	maxQueueSize = 2
}

func TestABRHandleSubmitRejectsInvalidMethod(t *testing.T) {
	resetABRTestState()

	req := httptest.NewRequest(http.MethodGet, "/v1/video/transcode/abr", nil)
	rec := httptest.NewRecorder()

	handleSubmit(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestABRHandleSubmitRequiresManifest(t *testing.T) {
	resetABRTestState()

	body := bytes.NewBufferString(`{"input_url":"http://example.com/in.mp4","preset":"abr-test","output_urls":{"renditions":{"360p":{"playlist":"http://example.com/360p.m3u8","stream":"http://example.com/360p.mp4"}}}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/video/transcode/abr", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handleSubmit(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if got := rec.Body.String(); !bytes.Contains([]byte(got), []byte("output_urls.manifest is required")) {
		t.Fatalf("body = %q, want manifest validation error", got)
	}
}

func TestABRHandleSubmitRequiresRenditionURLs(t *testing.T) {
	resetABRTestState()

	body := bytes.NewBufferString(`{"input_url":"http://example.com/in.mp4","preset":"abr-test","output_urls":{"manifest":"http://example.com/master.m3u8","renditions":{}}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/video/transcode/abr", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handleSubmit(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if got := rec.Body.String(); !bytes.Contains([]byte(got), []byte("missing output_urls for rendition")) {
		t.Fatalf("body = %q, want rendition validation error", got)
	}
}

func TestABRHandleStatusReturnsJob(t *testing.T) {
	resetABRTestState()

	job := &ABRJob{
		ID:              "abr-job-123",
		Status:          "complete",
		Phase:           "complete",
		OverallProgress: 100,
		Request: ABRRequest{
			OutputURLs: ABROutputURLs{Manifest: "http://example.com/master.m3u8"},
		},
		Renditions: []renditionState{{Name: "360p", Status: "complete", Progress: 100}},
		CreatedAt:  time.Unix(1700000000, 0),
		StartedAt:  time.Unix(1700000000, 0),
	}

	jobsMu.Lock()
	jobs[job.ID] = job
	jobsMu.Unlock()

	body := bytes.NewBufferString(`{"job_id":"abr-job-123"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/video/transcode/abr/status", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handleStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp ABRJobStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.JobID != job.ID {
		t.Fatalf("job_id = %q, want %q", resp.JobID, job.ID)
	}
	if resp.ManifestURL != "http://example.com/master.m3u8" {
		t.Fatalf("manifest_url = %q, want %q", resp.ManifestURL, "http://example.com/master.m3u8")
	}
}

func TestABRHandleHealthzReportsState(t *testing.T) {
	resetABRTestState()
	hw = transcode.HWProfile{GPUName: "Intel Arc", VRAM_MB: 8192}
	activeJobs.Store(1)

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
	if resp["gpu"] != "Intel Arc" {
		t.Fatalf("gpu = %v, want Intel Arc", resp["gpu"])
	}
	if got := int(resp["presets"].(float64)); got != 1 {
		t.Fatalf("presets = %d, want 1", got)
	}
}
