package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

type storeReporterV2 struct {
	store      *FileWorkloadStoreV2
	workloadID string
}

func (r storeReporterV2) Progress(phase string, progress float64, rendition string, frames uint64) error {
	_, err := r.store.AppendProgress(r.workloadID, phase, progress, rendition, frames)
	return err
}
func (r storeReporterV2) Prepared(value PreparedRenditionV2) error {
	return r.store.SavePrepared(r.workloadID, value)
}
func (r storeReporterV2) Delivered(value RenditionResultV2) error {
	return r.store.SaveDelivered(r.workloadID, value)
}
func (r storeReporterV2) PreparedManifest(value PreparedArtifactV2) error {
	return r.store.SavePreparedManifest(r.workloadID, value)
}
func (r storeReporterV2) DeliveredManifest(uri string) error {
	return r.store.SaveDeliveredManifest(r.workloadID, uri)
}

func TestDurableABRExecutorResumesDeliveredUploadsWithoutRepeatingThem(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileWorkloadStoreV2(dir)
	if err != nil {
		t.Fatal(err)
	}
	request := testABRRequestV2("asset-durable")
	var mu sync.Mutex
	puts := map[string]int{}
	uploads := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s", r.Method)
		}
		mu.Lock()
		puts[r.URL.Path]++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer uploads.Close()
	request.Output.Manifest.UploadURL = uploads.URL + "/master"
	destination := request.Output.Renditions["720p"]
	destination.Playlist.UploadURL = uploads.URL + "/playlist"
	destination.Stream.UploadURL = uploads.URL + "/stream"
	request.Output.Renditions["720p"] = destination
	hash, err := RequestContentSHA256V2(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LoadOrCreate(request.WorkloadID, hash); err != nil {
		t.Fatal(err)
	}

	workDir := filepath.Join(dir, "work", request.WorkloadID)
	renditionDir := filepath.Join(workDir, "720p")
	if err := os.MkdirAll(renditionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "input.mp4"), []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	playlist := []byte("#EXTM3U\n")
	stream := []byte("encoded-stream")
	if err := os.WriteFile(filepath.Join(renditionDir, "playlist.m3u8"), playlist, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(renditionDir, "stream.mp4"), stream, 0o600); err != nil {
		t.Fatal(err)
	}
	prepared := PreparedRenditionV2{
		Name: "720p", PlaylistPath: "work/asset-durable/720p/playlist.m3u8", StreamPath: "work/asset-durable/720p/stream.mp4",
		PlaylistSHA256: bytesSHA256V2(playlist), StreamSHA256: bytesSHA256V2(stream),
		Video: &DeliveredVideoV2{ActualFrames: 300, Width: 1280, Height: 720}, FileSizeBytes: uint64(len(stream)),
	}
	if err := store.SavePrepared(request.WorkloadID, prepared); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	ffprobe := filepath.Join(binDir, "ffprobe")
	if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\nprintf '%s' '{\"format\":{\"duration\":\"10\"},\"streams\":[{\"codec_type\":\"video\",\"width\":1280,\"height\":720}]}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	executor, err := NewDurableABRExecutorV2(store, transcode.HWProfile{})
	if err != nil {
		t.Fatal(err)
	}
	reporter := storeReporterV2{store: store, workloadID: request.WorkloadID}
	result, err := executor.Execute(context.Background(), request, testABRPresetV2(), reporter)
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage.Units != 277 || len(result.Renditions) != 1 {
		t.Fatalf("result = %+v", result)
	}
	// Simulate a crash before the coordinator writes the terminal event. Every
	// delivered PUT is already journaled, so execution resumes without a PUT.
	if _, err := executor.Execute(context.Background(), request, testABRPresetV2(), reporter); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/playlist", "/stream", "/master"} {
		if puts[path] != 1 {
			t.Fatalf("PUT %s count = %d, want 1", path, puts[path])
		}
	}
	journal, err := store.Load(request.WorkloadID)
	if err != nil || journal.ManifestDeliveredURI != request.Output.Manifest.ArtifactURI {
		t.Fatalf("manifest checkpoint = %q, %v", journal.ManifestDeliveredURI, err)
	}
}

func bytesSHA256V2(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}
