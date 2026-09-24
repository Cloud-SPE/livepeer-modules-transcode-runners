package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkloadStorePersistsReplayIdentityWithoutCredentials(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileWorkloadStoreV2(dir)
	if err != nil {
		t.Fatal(err)
	}
	record, disposition, err := store.LoadOrCreate("asset-001", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if disposition != ResumeExistingV2 || record.State != WorkloadInProgressV2 {
		t.Fatalf("new journal = %q/%q", disposition, record.State)
	}

	// A new store instance simulates a process restart.
	restarted, err := NewFileWorkloadStoreV2(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, disposition, err = restarted.LoadOrCreate("asset-001", strings.Repeat("a", 64))
	if err != nil || disposition != ResumeExistingV2 {
		t.Fatalf("identical restart replay = %q, %v", disposition, err)
	}
	_, disposition, err = restarted.LoadOrCreate("asset-001", strings.Repeat("b", 64))
	if err != nil || disposition != RejectIDReuseV2 {
		t.Fatalf("different restart replay = %q, %v", disposition, err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "asset-001.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"upload_url", "download_url", "sig="} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("journal contains credential marker %q", secret)
		}
	}
	info, err := os.Stat(filepath.Join(dir, "asset-001.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode = %o, want 600", info.Mode().Perm())
	}
}

func TestWorkloadStoreCheckpointsPreparedAndDeliveredRenditions(t *testing.T) {
	store, err := NewFileWorkloadStoreV2(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LoadOrCreate("asset-002", strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	prepared := PreparedRenditionV2{
		Name:           "720p",
		PlaylistPath:   "work/asset-002/720p/playlist.m3u8",
		StreamPath:     "work/asset-002/720p/stream.mp4",
		PlaylistSHA256: strings.Repeat("d", 64),
		StreamSHA256:   strings.Repeat("e", 64),
		Video:          &DeliveredVideoV2{ActualFrames: 300, Width: 1280, Height: 720},
		FileSizeBytes:  42,
	}
	if err := store.SavePrepared("asset-002", prepared); err != nil {
		t.Fatal(err)
	}
	delivered := RenditionResultV2{
		Name:          "720p",
		PlaylistURI:   "vod/asset-002/720p/playlist.m3u8",
		StreamURI:     "vod/asset-002/720p/stream.mp4",
		Video:         prepared.Video,
		FileSizeBytes: 42,
	}
	if err := store.SaveDelivered("asset-002", delivered); err != nil {
		t.Fatal(err)
	}
	record, err := store.Load("asset-002")
	if err != nil {
		t.Fatal(err)
	}
	if record.Prepared["720p"].StreamSHA256 != prepared.StreamSHA256 {
		t.Fatal("prepared checkpoint was not persisted")
	}
	if record.Delivered["720p"].PlaylistURI != delivered.PlaylistURI {
		t.Fatal("delivered checkpoint was not persisted")
	}
	if err := store.SavePrepared("asset-002", prepared); err == nil {
		t.Fatal("delivered checkpoint was replaceable")
	}
}

func TestWorkloadStoreProgressIsBoundedAndTerminalReplaysAfterRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileWorkloadStoreV2(dir)
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("f", 64)
	if _, _, err := store.LoadOrCreate("asset-003", hash); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxStoredProgressEventsV2+10; i++ {
		if _, err := store.AppendProgress("asset-003", "encoding", float64(i%100), "720p", uint64(i)); err != nil {
			t.Fatal(err)
		}
	}
	record, err := store.Load("asset-003")
	if err != nil {
		t.Fatal(err)
	}
	if len(record.ProgressEvents) != maxStoredProgressEventsV2 {
		t.Fatalf("stored progress count = %d", len(record.ProgressEvents))
	}

	terminal := ABRTerminalErrorV2{
		Schema:        ABRResultSchemaV2,
		WorkloadID:    "asset-003",
		RequestSHA256: hash,
		Outcome:       "failed",
		Error:         ABRErrorV2{Code: "canceled", Message: "execution interrupted", Retryable: true},
		Usage:         UsageClaimV2{Unit: ABRWorkUnitV2, Units: 0},
	}
	event, err := store.RecordTerminalError("asset-003", terminal)
	if err != nil {
		t.Fatal(err)
	}
	if event.Event != "error" {
		t.Fatalf("terminal event = %q", event.Event)
	}

	restarted, err := NewFileWorkloadStoreV2(dir)
	if err != nil {
		t.Fatal(err)
	}
	record, disposition, err := restarted.LoadOrCreate("asset-003", hash)
	if err != nil || disposition != ReplayTerminalV2 || record.TerminalEvent == nil {
		t.Fatalf("terminal restart replay = %q, terminal=%v, err=%v", disposition, record.TerminalEvent != nil, err)
	}
	var replayed ABRTerminalErrorV2
	if err := json.Unmarshal(record.TerminalEvent.Data, &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.Error.Code != "canceled" || replayed.Usage.Units != 0 {
		t.Fatalf("replayed terminal = %+v", replayed)
	}
	if _, err := store.AppendProgress("asset-003", "encoding", 50, "720p", 100); err == nil {
		t.Fatal("terminal workload accepted progress")
	}
}

func TestWorkloadStoreFailsClosedOnCorruptJournal(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileWorkloadStoreV2(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "asset-004.json"), []byte(`{"version":1,"workload_id":"wrong"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("asset-004"); err == nil {
		t.Fatal("corrupt journal was accepted")
	}
}

func TestTerminalErrorRejectsCredentialLeak(t *testing.T) {
	err := ValidateABRTerminalErrorV2(ABRTerminalErrorV2{
		Schema:        ABRResultSchemaV2,
		WorkloadID:    "asset-005",
		RequestSHA256: strings.Repeat("a", 64),
		Outcome:       "failed",
		Error:         ABRErrorV2{Code: "upload_failed", Message: "PUT https://storage.example/out?sig=secret failed"},
		Usage:         UsageClaimV2{Unit: ABRWorkUnitV2, Units: 0},
	})
	if err == nil {
		t.Fatal("credential-bearing error was accepted")
	}
}
