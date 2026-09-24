package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const workloadJournalVersionV2 = 1
const maxStoredProgressEventsV2 = 256

type WorkloadJournalStateV2 string

const (
	WorkloadInProgressV2 WorkloadJournalStateV2 = "in_progress"
	WorkloadSucceededV2  WorkloadJournalStateV2 = "succeeded"
	WorkloadFailedV2     WorkloadJournalStateV2 = "failed"
)

type PreparedRenditionV2 struct {
	Name           string            `json:"name"`
	PlaylistPath   string            `json:"playlist_path"`
	StreamPath     string            `json:"stream_path"`
	PlaylistSHA256 string            `json:"playlist_sha256"`
	StreamSHA256   string            `json:"stream_sha256"`
	Video          *DeliveredVideoV2 `json:"video,omitempty"`
	FileSizeBytes  uint64            `json:"file_size_bytes"`
}

type PreparedArtifactV2 struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type StoredSSEEventV2 struct {
	Sequence uint64          `json:"sequence"`
	Event    string          `json:"event"`
	Data     json.RawMessage `json:"data"`
}

type WorkloadJournalV2 struct {
	Version              int                            `json:"version"`
	WorkloadID           string                         `json:"workload_id"`
	RequestSHA256        string                         `json:"request_sha256"`
	State                WorkloadJournalStateV2         `json:"state"`
	Phase                string                         `json:"phase"`
	NextSequence         uint64                         `json:"next_sequence"`
	Prepared             map[string]PreparedRenditionV2 `json:"prepared"`
	Delivered            map[string]RenditionResultV2   `json:"delivered"`
	ManifestPrepared     *PreparedArtifactV2            `json:"manifest_prepared,omitempty"`
	ManifestDeliveredURI string                         `json:"manifest_delivered_uri,omitempty"`
	ProgressEvents       []StoredSSEEventV2             `json:"progress_events"`
	TerminalEvent        *StoredSSEEventV2              `json:"terminal_event,omitempty"`
}

type FileWorkloadStoreV2 struct {
	dir string
	mu  sync.Mutex
}

func NewFileWorkloadStoreV2(dir string) (*FileWorkloadStoreV2, error) {
	if dir == "" {
		return nil, errors.New("workload state directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create workload state directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure workload state directory: %w", err)
	}
	return &FileWorkloadStoreV2{dir: dir}, nil
}

func (s *FileWorkloadStoreV2) LoadOrCreate(workloadID, requestSHA256 string) (WorkloadJournalV2, WorkloadReplayDispositionV2, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !workloadIDPattern.MatchString(workloadID) || !sha256Pattern.MatchString(requestSHA256) {
		return WorkloadJournalV2{}, "", errors.New("invalid workload journal identity")
	}
	record, err := s.loadLocked(workloadID)
	if errors.Is(err, os.ErrNotExist) {
		record = WorkloadJournalV2{
			Version:       workloadJournalVersionV2,
			WorkloadID:    workloadID,
			RequestSHA256: requestSHA256,
			State:         WorkloadInProgressV2,
			Phase:         "accepted",
			NextSequence:  1,
			Prepared:      map[string]PreparedRenditionV2{},
			Delivered:     map[string]RenditionResultV2{},
		}
		if err := s.saveLocked(record); err != nil {
			return WorkloadJournalV2{}, "", err
		}
		return cloneJournalV2(record), ResumeExistingV2, nil
	}
	if err != nil {
		return WorkloadJournalV2{}, "", err
	}
	terminal := record.State == WorkloadSucceededV2 || record.State == WorkloadFailedV2
	disposition := ClassifyWorkloadReplayV2(record.RequestSHA256, requestSHA256, terminal)
	return cloneJournalV2(record), disposition, nil
}

func (s *FileWorkloadStoreV2) Load(workloadID string) (WorkloadJournalV2, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.loadLocked(workloadID)
	return cloneJournalV2(record), err
}

func (s *FileWorkloadStoreV2) AppendProgress(workloadID, phase string, progress float64, rendition string, frames uint64) (StoredSSEEventV2, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.loadLocked(workloadID)
	if err != nil {
		return StoredSSEEventV2{}, err
	}
	if record.State != WorkloadInProgressV2 {
		return StoredSSEEventV2{}, errors.New("cannot append progress to terminal workload")
	}
	eventValue := ABRProgressV2{
		Schema:          ABRProgressSchemaV2,
		WorkloadID:      record.WorkloadID,
		RequestSHA256:   record.RequestSHA256,
		Sequence:        record.NextSequence,
		Phase:           phase,
		OverallProgress: progress,
		Rendition:       rendition,
		Frames:          frames,
	}
	if err := ValidateABRProgressV2(eventValue); err != nil {
		return StoredSSEEventV2{}, err
	}
	event, err := storedEventV2(record.NextSequence, "progress", eventValue)
	if err != nil {
		return StoredSSEEventV2{}, err
	}
	record.NextSequence++
	record.Phase = phase
	record.ProgressEvents = append(record.ProgressEvents, event)
	if len(record.ProgressEvents) > maxStoredProgressEventsV2 {
		record.ProgressEvents = append([]StoredSSEEventV2(nil), record.ProgressEvents[len(record.ProgressEvents)-maxStoredProgressEventsV2:]...)
	}
	if err := s.saveLocked(record); err != nil {
		return StoredSSEEventV2{}, err
	}
	return event, nil
}

func (s *FileWorkloadStoreV2) SavePrepared(workloadID string, prepared PreparedRenditionV2) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.loadLocked(workloadID)
	if err != nil {
		return err
	}
	if record.State != WorkloadInProgressV2 || prepared.Name == "" || prepared.PlaylistPath == "" || prepared.StreamPath == "" ||
		!safeJournalPathV2(prepared.PlaylistPath) || !safeJournalPathV2(prepared.StreamPath) ||
		!sha256Pattern.MatchString(prepared.PlaylistSHA256) || !sha256Pattern.MatchString(prepared.StreamSHA256) ||
		(prepared.Video != nil && (prepared.Video.ActualFrames == 0 || prepared.Video.Width == 0 || prepared.Video.Height == 0)) {
		return errors.New("invalid prepared rendition checkpoint")
	}
	if _, delivered := record.Delivered[prepared.Name]; delivered {
		return errors.New("cannot replace a delivered rendition checkpoint")
	}
	record.Prepared[prepared.Name] = prepared
	return s.saveLocked(record)
}

func (s *FileWorkloadStoreV2) SaveDelivered(workloadID string, result RenditionResultV2) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.loadLocked(workloadID)
	if err != nil {
		return err
	}
	if record.State != WorkloadInProgressV2 {
		return errors.New("cannot deliver a rendition for terminal workload")
	}
	prepared, ok := record.Prepared[result.Name]
	if !ok || !preparedMatchesResultV2(prepared, result) {
		return errors.New("delivered rendition has no matching prepared checkpoint")
	}
	if err := validateArtifactURI(result.PlaylistURI, "playlist_uri"); err != nil {
		return err
	}
	if err := validateArtifactURI(result.StreamURI, "stream_uri"); err != nil {
		return err
	}
	record.Delivered[result.Name] = result
	return s.saveLocked(record)
}

func (s *FileWorkloadStoreV2) SavePreparedManifest(workloadID string, prepared PreparedArtifactV2) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.loadLocked(workloadID)
	if err != nil {
		return err
	}
	if record.State != WorkloadInProgressV2 || record.ManifestDeliveredURI != "" || !safeJournalPathV2(prepared.Path) || !sha256Pattern.MatchString(prepared.SHA256) {
		return errors.New("invalid prepared manifest checkpoint")
	}
	record.ManifestPrepared = &prepared
	return s.saveLocked(record)
}

func (s *FileWorkloadStoreV2) SaveDeliveredManifest(workloadID, artifactURI string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.loadLocked(workloadID)
	if err != nil {
		return err
	}
	if record.State != WorkloadInProgressV2 || record.ManifestPrepared == nil {
		return errors.New("manifest has no prepared checkpoint")
	}
	if err := validateArtifactURI(artifactURI, "manifest_uri"); err != nil {
		return err
	}
	record.ManifestDeliveredURI = artifactURI
	return s.saveLocked(record)
}

func (s *FileWorkloadStoreV2) RecordTerminalResult(workloadID string, result ABRTerminalResultV2) (StoredSSEEventV2, error) {
	if err := ValidateABRTerminalResultV2(result); err != nil {
		return StoredSSEEventV2{}, err
	}
	return s.recordTerminal(workloadID, WorkloadSucceededV2, "result", result)
}

func (s *FileWorkloadStoreV2) RecordTerminalError(workloadID string, result ABRTerminalErrorV2) (StoredSSEEventV2, error) {
	if err := ValidateABRTerminalErrorV2(result); err != nil {
		return StoredSSEEventV2{}, err
	}
	return s.recordTerminal(workloadID, WorkloadFailedV2, "error", result)
}

func (s *FileWorkloadStoreV2) recordTerminal(workloadID string, state WorkloadJournalStateV2, eventName string, value any) (StoredSSEEventV2, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.loadLocked(workloadID)
	if err != nil {
		return StoredSSEEventV2{}, err
	}
	if record.State != WorkloadInProgressV2 {
		return StoredSSEEventV2{}, errors.New("workload is already terminal")
	}
	switch result := value.(type) {
	case ABRTerminalResultV2:
		if result.WorkloadID != record.WorkloadID || result.RequestSHA256 != record.RequestSHA256 {
			return StoredSSEEventV2{}, errors.New("terminal result identity differs from workload journal")
		}
	case ABRTerminalErrorV2:
		if result.WorkloadID != record.WorkloadID || result.RequestSHA256 != record.RequestSHA256 {
			return StoredSSEEventV2{}, errors.New("terminal error identity differs from workload journal")
		}
		delivered := make([]RenditionResultV2, 0, len(record.Delivered))
		for _, rendition := range record.Delivered {
			delivered = append(delivered, rendition)
		}
		units, err := CalculateFrameMegapixelUnitsV2(delivered)
		if err != nil {
			return StoredSSEEventV2{}, err
		}
		if result.Usage.Units != units {
			return StoredSSEEventV2{}, fmt.Errorf("terminal error usage differs from delivered checkpoints: got %d want %d", result.Usage.Units, units)
		}
	default:
		return StoredSSEEventV2{}, errors.New("unsupported terminal event type")
	}
	if result, ok := value.(ABRTerminalResultV2); ok {
		if result.ManifestURI != record.ManifestDeliveredURI {
			return StoredSSEEventV2{}, errors.New("terminal result differs from delivered manifest checkpoint")
		}
		if len(result.Renditions) != len(record.Delivered) {
			return StoredSSEEventV2{}, errors.New("terminal result does not match delivered checkpoints")
		}
		for _, rendition := range result.Renditions {
			checkpoint, exists := record.Delivered[rendition.Name]
			if !exists || !renditionResultsEqualV2(checkpoint, rendition) {
				return StoredSSEEventV2{}, errors.New("terminal result differs from delivered checkpoint")
			}
		}
	}
	event, err := storedEventV2(record.NextSequence, eventName, value)
	if err != nil {
		return StoredSSEEventV2{}, err
	}
	record.NextSequence++
	record.State = state
	record.Phase = string(state)
	record.TerminalEvent = &event
	if err := s.saveLocked(record); err != nil {
		return StoredSSEEventV2{}, err
	}
	return event, nil
}

func (s *FileWorkloadStoreV2) Recoverable() ([]WorkloadJournalV2, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var result []WorkloadJournalV2
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		workloadID := entry.Name()[:len(entry.Name())-len(".json")]
		record, err := s.loadLocked(workloadID)
		if err != nil {
			return nil, err
		}
		if record.State == WorkloadInProgressV2 {
			result = append(result, cloneJournalV2(record))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].WorkloadID < result[j].WorkloadID })
	return result, nil
}

func (s *FileWorkloadStoreV2) loadLocked(workloadID string) (WorkloadJournalV2, error) {
	if !workloadIDPattern.MatchString(workloadID) {
		return WorkloadJournalV2{}, errors.New("invalid workload ID")
	}
	body, err := os.ReadFile(s.path(workloadID))
	if err != nil {
		return WorkloadJournalV2{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var record WorkloadJournalV2
	if err := decoder.Decode(&record); err != nil {
		return WorkloadJournalV2{}, fmt.Errorf("decode workload journal %q: %w", workloadID, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return WorkloadJournalV2{}, fmt.Errorf("decode workload journal %q: trailing JSON", workloadID)
	}
	if record.Version != workloadJournalVersionV2 || record.WorkloadID != workloadID || !sha256Pattern.MatchString(record.RequestSHA256) {
		return WorkloadJournalV2{}, fmt.Errorf("workload journal %q failed identity validation", workloadID)
	}
	if record.Prepared == nil {
		record.Prepared = map[string]PreparedRenditionV2{}
	}
	if record.Delivered == nil {
		record.Delivered = map[string]RenditionResultV2{}
	}
	if err := s.validateJournalLocked(record); err != nil {
		return WorkloadJournalV2{}, fmt.Errorf("workload journal %q is invalid: %w", workloadID, err)
	}
	return record, nil
}

func (s *FileWorkloadStoreV2) saveLocked(record WorkloadJournalV2) error {
	temporary, err := os.CreateTemp(s.dir, ".journal-*.tmp")
	if err != nil {
		return fmt.Errorf("create workload journal temp file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	if err := encoder.Encode(record); err != nil {
		temporary.Close()
		return fmt.Errorf("encode workload journal: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync workload journal: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close workload journal: %w", err)
	}
	if err := os.Rename(temporaryName, s.path(record.WorkloadID)); err != nil {
		return fmt.Errorf("commit workload journal: %w", err)
	}
	directory, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (s *FileWorkloadStoreV2) path(workloadID string) string {
	return filepath.Join(s.dir, workloadID+".json")
}

func (s *FileWorkloadStoreV2) validateJournalLocked(record WorkloadJournalV2) error {
	if record.NextSequence == 0 {
		return errors.New("next event sequence must be positive")
	}
	terminal := record.State == WorkloadSucceededV2 || record.State == WorkloadFailedV2
	if record.State != WorkloadInProgressV2 && !terminal {
		return errors.New("unknown workload state")
	}
	if terminal != (record.TerminalEvent != nil) {
		return errors.New("terminal state and event disagree")
	}
	if record.TerminalEvent != nil && (record.TerminalEvent.Sequence >= record.NextSequence || (record.TerminalEvent.Event != "result" && record.TerminalEvent.Event != "error")) {
		return errors.New("terminal event identity is invalid")
	}
	for name, prepared := range record.Prepared {
		if name != prepared.Name || !safeJournalPathV2(prepared.PlaylistPath) || !safeJournalPathV2(prepared.StreamPath) ||
			!sha256Pattern.MatchString(prepared.PlaylistSHA256) || !sha256Pattern.MatchString(prepared.StreamSHA256) {
			return errors.New("prepared checkpoint is invalid")
		}
	}
	for name, delivered := range record.Delivered {
		prepared, ok := record.Prepared[name]
		if name != delivered.Name || !ok || !preparedMatchesResultV2(prepared, delivered) {
			return errors.New("delivered checkpoint is invalid")
		}
	}
	if record.ManifestPrepared != nil && (!safeJournalPathV2(record.ManifestPrepared.Path) || !sha256Pattern.MatchString(record.ManifestPrepared.SHA256)) {
		return errors.New("prepared manifest checkpoint is invalid")
	}
	if record.ManifestDeliveredURI != "" {
		if record.ManifestPrepared == nil {
			return errors.New("delivered manifest has no prepared checkpoint")
		}
		if err := validateArtifactURI(record.ManifestDeliveredURI, "manifest_delivered_uri"); err != nil {
			return err
		}
	}
	return nil
}

func storedEventV2(sequence uint64, eventName string, value any) (StoredSSEEventV2, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return StoredSSEEventV2{}, err
	}
	return StoredSSEEventV2{Sequence: sequence, Event: eventName, Data: body}, nil
}

func cloneJournalV2(record WorkloadJournalV2) WorkloadJournalV2 {
	body, err := json.Marshal(record)
	if err != nil {
		return WorkloadJournalV2{}
	}
	var cloned WorkloadJournalV2
	if err := json.Unmarshal(body, &cloned); err != nil {
		return WorkloadJournalV2{}
	}
	return cloned
}

func safeJournalPathV2(path string) bool {
	clean := filepath.Clean(path)
	return path != "" && path == clean && !filepath.IsAbs(path) && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func preparedMatchesResultV2(prepared PreparedRenditionV2, result RenditionResultV2) bool {
	if prepared.Name != result.Name || prepared.FileSizeBytes != result.FileSizeBytes || (prepared.Video == nil) != (result.Video == nil) {
		return false
	}
	if prepared.Video == nil {
		return true
	}
	return *prepared.Video == *result.Video
}

func renditionResultsEqualV2(left, right RenditionResultV2) bool {
	return left.PlaylistURI == right.PlaylistURI && left.StreamURI == right.StreamURI && preparedMatchesResultV2(PreparedRenditionV2{
		Name:          left.Name,
		Video:         left.Video,
		FileSizeBytes: left.FileSizeBytes,
	}, right)
}
