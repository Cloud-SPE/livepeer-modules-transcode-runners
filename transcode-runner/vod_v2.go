package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

const (
	VODRequestSchemaV2    = "video-transcode-vod/v2"
	VODProgressSchemaV2   = "video-transcode-vod-progress/v2"
	VODResultSchemaV2     = "video-transcode-vod-result/v2"
	VODWorkUnitV2         = "video-frame-megapixel"
	VODWorkUnitsTrailerV2 = "X-Livepeer-Work-Units"
	maxVODBodyV2          = 5 << 20
)

var vodWorkloadIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type VODRequestV2 struct {
	Schema     string         `json:"schema"`
	WorkloadID string         `json:"workload_id"`
	Input      VODInputV2     `json:"input"`
	Rendition  VODRenditionV2 `json:"rendition"`
	Output     VODOutputV2    `json:"output"`
}

type VODInputV2 struct {
	DownloadURL   string `json:"download_url"`
	ContentSHA256 string `json:"content_sha256,omitempty"`
}

type VODRenditionV2 struct {
	Name   string `json:"name"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	FPS    int    `json:"fps"`
	Codec  string `json:"codec,omitempty"`
}

type VODDestinationV2 struct {
	ArtifactURI string `json:"artifact_uri"`
	UploadURL   string `json:"upload_url"`
}

type VODOutputV2 struct {
	Stream VODDestinationV2 `json:"stream"`
}

type VODUsageV2 struct {
	Unit  string `json:"unit"`
	Units uint64 `json:"units"`
}

type VODVideoV2 struct {
	ActualFrames uint64 `json:"actual_frames"`
	Width        uint32 `json:"width"`
	Height       uint32 `json:"height"`
}

type VODProgressV2 struct {
	Schema          string  `json:"schema"`
	WorkloadID      string  `json:"workload_id"`
	RequestSHA256   string  `json:"request_sha256"`
	Sequence        uint64  `json:"sequence"`
	Phase           string  `json:"phase"`
	OverallProgress float64 `json:"overall_progress"`
	Frames          uint64  `json:"frames,omitempty"`
}

type VODResultV2 struct {
	Schema        string      `json:"schema"`
	WorkloadID    string      `json:"workload_id"`
	RequestSHA256 string      `json:"request_sha256"`
	Outcome       string      `json:"outcome"`
	Name          string      `json:"name"`
	StreamURI     string      `json:"stream_uri"`
	Video         *VODVideoV2 `json:"video,omitempty"`
	FileSizeBytes uint64      `json:"file_size_bytes"`
	Usage         VODUsageV2  `json:"usage"`
}

type VODErrorBodyV2 struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type VODErrorV2 struct {
	Schema        string         `json:"schema"`
	WorkloadID    string         `json:"workload_id"`
	RequestSHA256 string         `json:"request_sha256"`
	Outcome       string         `json:"outcome"`
	Error         VODErrorBodyV2 `json:"error"`
	Usage         VODUsageV2     `json:"usage"`
}

type vodEventV2 struct {
	Sequence uint64          `json:"sequence"`
	Event    string          `json:"event"`
	Data     json.RawMessage `json:"data"`
}

type vodJournalV2 struct {
	Version       int          `json:"version"`
	WorkloadID    string       `json:"workload_id"`
	RequestSHA256 string       `json:"request_sha256"`
	State         string       `json:"state"`
	NextSequence  uint64       `json:"next_sequence"`
	Events        []vodEventV2 `json:"events"`
	PreparedPath  string       `json:"prepared_path,omitempty"`
	PreparedHash  string       `json:"prepared_hash,omitempty"`
	Video         *VODVideoV2  `json:"video,omitempty"`
	FileSizeBytes uint64       `json:"file_size_bytes,omitempty"`
	Delivered     bool         `json:"delivered,omitempty"`
}

type VODStoreV2 struct {
	dir string
	mu  sync.Mutex
}

func NewVODStoreV2(dir string) (*VODStoreV2, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("state directory is required")
	}
	if err := os.MkdirAll(filepath.Join(dir, "journals"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o700); err != nil {
		return nil, err
	}
	return &VODStoreV2{dir: dir}, nil
}

func (s *VODStoreV2) loadOrCreate(id, hash string) (vodJournalV2, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.loadLocked(id)
	if errors.Is(err, os.ErrNotExist) {
		j = vodJournalV2{Version: 1, WorkloadID: id, RequestSHA256: hash, State: "in_progress", NextSequence: 1}
		return j, "new", s.saveLocked(j)
	}
	if err != nil {
		return j, "", err
	}
	if j.RequestSHA256 != hash {
		return j, "reuse", nil
	}
	if j.State != "in_progress" {
		return j, "terminal", nil
	}
	return j, "resume", nil
}

func (s *VODStoreV2) load(id string) (vodJournalV2, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(id)
}

func (s *VODStoreV2) loadLocked(id string) (vodJournalV2, error) {
	var j vodJournalV2
	body, err := os.ReadFile(filepath.Join(s.dir, "journals", id+".json"))
	if err != nil {
		return j, err
	}
	if err := json.Unmarshal(body, &j); err != nil {
		return j, err
	}
	if j.WorkloadID != id || j.Version != 1 {
		return j, errors.New("invalid workload journal")
	}
	return j, nil
}

func (s *VODStoreV2) saveLocked(j vodJournalV2) error {
	body, err := json.Marshal(j)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, "journals", j.WorkloadID+".json")
	tmp, err := os.CreateTemp(filepath.Dir(path), ".journal-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func makeVODEventV2(seq uint64, name string, value any) (vodEventV2, error) {
	body, err := json.Marshal(value)
	return vodEventV2{Sequence: seq, Event: name, Data: body}, err
}

func (s *VODStoreV2) progress(id, phase string, percent float64, frames uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.loadLocked(id)
	if err != nil {
		return err
	}
	if j.State != "in_progress" {
		return errors.New("terminal workload")
	}
	e, err := makeVODEventV2(j.NextSequence, "progress", VODProgressV2{Schema: VODProgressSchemaV2, WorkloadID: id, RequestSHA256: j.RequestSHA256, Sequence: j.NextSequence, Phase: phase, OverallProgress: percent, Frames: frames})
	if err != nil {
		return err
	}
	j.NextSequence++
	j.Events = append(j.Events, e)
	if len(j.Events) > 256 {
		j.Events = append([]vodEventV2(nil), j.Events[len(j.Events)-256:]...)
	}
	return s.saveLocked(j)
}

func (s *VODStoreV2) prepared(id, path, hash string, video *VODVideoV2, size uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.loadLocked(id)
	if err != nil {
		return err
	}
	if j.State != "in_progress" {
		return errors.New("terminal workload")
	}
	j.PreparedPath, j.PreparedHash, j.Video, j.FileSizeBytes = path, hash, video, size
	return s.saveLocked(j)
}

func (s *VODStoreV2) delivered(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.loadLocked(id)
	if err != nil {
		return err
	}
	j.Delivered = true
	return s.saveLocked(j)
}

func (s *VODStoreV2) terminal(id, state, event string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.loadLocked(id)
	if err != nil {
		return err
	}
	if j.State != "in_progress" {
		return errors.New("terminal workload")
	}
	e, err := makeVODEventV2(j.NextSequence, event, value)
	if err != nil {
		return err
	}
	j.NextSequence++
	j.Events = append(j.Events, e)
	j.State = state
	return s.saveLocked(j)
}

type vodRunV2 struct {
	hash     string
	done     chan struct{}
	gpuLease *transcode.GPUAdmissionLease
}

type VODServiceV2 struct {
	store                *VODStoreV2
	presets              []transcode.Preset
	hw                   transcode.HWProfile
	limit                int
	gpuAdmission         *transcode.GPUAdmissionGate
	mu                   sync.Mutex
	runs                 map[string]*vodRunV2
	active               atomic.Int32
	gpuAdmissionRejected atomic.Uint64
}

func NewVODServiceV2(store *VODStoreV2, presets []transcode.Preset, hw transcode.HWProfile, limit int, admission *transcode.GPUAdmissionGate) (*VODServiceV2, error) {
	if store == nil || len(presets) == 0 || limit < 1 {
		return nil, errors.New("store, presets, and positive limit are required")
	}
	return &VODServiceV2{store: store, presets: presets, hw: hw, limit: limit, gpuAdmission: admission, runs: map[string]*vodRunV2{}}, nil
}

func (s *VODServiceV2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		writeJSON(w, http.StatusNotAcceptable, map[string]string{"error": "sse_transport_required"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxVODBodyV2)
	var req VODRequestV2
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid_request", "message": err.Error()})
		return
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSON(w, 400, map[string]string{"error": "invalid_request"})
		return
	}
	preset, err := s.validateAndResolve(req)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid_request", "message": err.Error()})
		return
	}
	hash, err := vodRequestHashV2(req)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "request_hash_failed"})
		return
	}
	j, disposition, err := s.store.loadOrCreate(req.WorkloadID, hash)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "journal_unavailable"})
		return
	}
	if disposition == "reuse" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "workload_id_reuse"})
		return
	}
	run, err := s.subscribe(req, preset, hash, disposition)
	if errors.Is(err, errVODCapacity) {
		writeJSON(w, 429, map[string]string{"error": "capacity_reached"})
		return
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "execution_unavailable"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Add("Trailer", VODWorkUnitsTrailerV2)
	w.WriteHeader(200)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	flusher.Flush()
	last := uint64(0)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		j, err = s.store.load(req.WorkloadID)
		if err != nil {
			return
		}
		for _, event := range j.Events {
			if event.Sequence > last {
				if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Sequence, event.Event, event.Data); err != nil {
					return
				}
				last = event.Sequence
				flusher.Flush()
			}
		}
		if j.State != "in_progress" {
			w.Header().Set(VODWorkUnitsTrailerV2, strconv.FormatUint(vodTerminalUnitsV2(j), 10))
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-run.done:
		case <-ticker.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

var errVODCapacity = errors.New("capacity reached")

func (s *VODServiceV2) subscribe(req VODRequestV2, preset transcode.Preset, hash, disposition string) (*vodRunV2, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if disposition == "terminal" {
		closed := make(chan struct{})
		close(closed)
		return &vodRunV2{hash: hash, done: closed}, nil
	}
	if run := s.runs[req.WorkloadID]; run != nil {
		if run.hash != hash {
			return nil, errors.New("identity mismatch")
		}
		return run, nil
	}
	if len(s.runs) >= s.limit {
		return nil, errVODCapacity
	}
	lease, err := s.gpuAdmission.Acquire(transcode.GPUAdmissionBatch)
	if errors.Is(err, transcode.ErrGPUAdmissionCapacity) {
		s.gpuAdmissionRejected.Add(1)
		return nil, errVODCapacity
	}
	if err != nil {
		return nil, err
	}
	run := &vodRunV2{hash: hash, done: make(chan struct{}), gpuLease: lease}
	s.runs[req.WorkloadID] = run
	s.active.Add(1)
	go s.execute(req, preset, hash, run)
	return run, nil
}

func (s *VODServiceV2) execute(req VODRequestV2, preset transcode.Preset, hash string, run *vodRunV2) {
	defer func() {
		_ = run.gpuLease.Close()
		s.mu.Lock()
		delete(s.runs, req.WorkloadID)
		close(run.done)
		s.active.Add(-1)
		s.mu.Unlock()
	}()
	result, err := s.doExecute(context.Background(), req, preset, hash)
	if err == nil {
		_ = s.store.terminal(req.WorkloadID, "succeeded", "result", result)
		return
	}
	_ = s.store.terminal(req.WorkloadID, "failed", "error", VODErrorV2{Schema: VODResultSchemaV2, WorkloadID: req.WorkloadID, RequestSHA256: hash, Outcome: "failed", Error: VODErrorBodyV2{Code: "execution_failed", Message: "VOD execution failed", Retryable: true}, Usage: VODUsageV2{Unit: VODWorkUnitV2, Units: 0}})
}

func (s *VODServiceV2) doExecute(ctx context.Context, req VODRequestV2, preset transcode.Preset, hash string) (VODResultV2, error) {
	workDir := filepath.Join(s.store.dir, "work", req.WorkloadID)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return VODResultV2{}, err
	}
	inputPath := filepath.Join(workDir, "input"+guessExtension(req.Input.DownloadURL))
	outputPath := filepath.Join(workDir, "output.mp4")
	if _, err := os.Stat(inputPath); err != nil {
		_ = s.store.progress(req.WorkloadID, "downloading", 0, 0)
		partial := inputPath + ".partial"
		_ = os.Remove(partial)
		if _, err := transcode.DownloadFile(ctx, req.Input.DownloadURL, partial, nil); err != nil {
			return VODResultV2{}, err
		}
		if req.Input.ContentSHA256 != "" {
			got, _ := vodFileHashV2(partial)
			if got != req.Input.ContentSHA256 {
				return VODResultV2{}, errors.New("input hash mismatch")
			}
		}
		if err := os.Rename(partial, inputPath); err != nil {
			return VODResultV2{}, err
		}
	}
	_ = s.store.progress(req.WorkloadID, "probing", 5, 0)
	probeOutput, err := exec.CommandContext(ctx, "ffprobe", "-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", inputPath).Output()
	if err != nil {
		return VODResultV2{}, err
	}
	probe, err := transcode.ParseProbeOutput(probeOutput)
	if err != nil {
		return VODResultV2{}, err
	}
	j, err := s.store.load(req.WorkloadID)
	if err != nil {
		return VODResultV2{}, err
	}
	if j.PreparedPath == "" || !vodPreparedValidV2(s.store.dir, j) {
		_ = os.Remove(outputPath)
		_ = s.store.progress(req.WorkloadID, "encoding", 10, 0)
		cmd := transcode.TranscodeCmd(inputPath, outputPath, preset, s.hw, probe, transcode.TranscodeOptions{ToneMap: probe.IsHDR()})
		cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return VODResultV2{}, err
		}
		if err := cmd.Start(); err != nil {
			return VODResultV2{}, err
		}
		scanner := bufio.NewScanner(stderr)
		scanner.Split(scanFFmpegLines)
		last := time.Time{}
		for scanner.Scan() {
			info, ok := transcode.ParseProgressLine(scanner.Text())
			if ok && time.Since(last) >= time.Second {
				last = time.Now()
				pct := 10 + transcode.CalcPercent(info.Time, time.Duration(probe.Duration*float64(time.Second)))*0.8
				_ = s.store.progress(req.WorkloadID, "encoding", pct, uint64(max(info.Frame, 0)))
			}
		}
		if err := cmd.Wait(); err != nil {
			return VODResultV2{}, err
		}
		if err := scanner.Err(); err != nil {
			return VODResultV2{}, err
		}
		video, err := vodExactVideoV2(ctx, outputPath)
		if err != nil {
			return VODResultV2{}, err
		}
		fileHash, err := vodFileHashV2(outputPath)
		if err != nil {
			return VODResultV2{}, err
		}
		stat, err := os.Stat(outputPath)
		if err != nil {
			return VODResultV2{}, err
		}
		rel, _ := filepath.Rel(s.store.dir, outputPath)
		if err := s.store.prepared(req.WorkloadID, rel, fileHash, video, uint64(stat.Size())); err != nil {
			return VODResultV2{}, err
		}
		j, _ = s.store.load(req.WorkloadID)
	}
	if !j.Delivered {
		_ = s.store.progress(req.WorkloadID, "uploading", 95, j.Video.ActualFrames)
		if err := transcode.UploadFile(ctx, filepath.Join(s.store.dir, j.PreparedPath), req.Output.Stream.UploadURL, nil); err != nil {
			return VODResultV2{}, err
		}
		if err := s.store.delivered(req.WorkloadID); err != nil {
			return VODResultV2{}, err
		}
	}
	units, err := vodUnitsV2(j.Video)
	if err != nil {
		return VODResultV2{}, err
	}
	return VODResultV2{Schema: VODResultSchemaV2, WorkloadID: req.WorkloadID, RequestSHA256: hash, Outcome: "succeeded", Name: req.Rendition.Name, StreamURI: req.Output.Stream.ArtifactURI, Video: j.Video, FileSizeBytes: j.FileSizeBytes, Usage: VODUsageV2{Unit: VODWorkUnitV2, Units: units}}, nil
}

func (s *VODServiceV2) validateAndResolve(req VODRequestV2) (transcode.Preset, error) {
	if req.Schema != VODRequestSchemaV2 {
		return transcode.Preset{}, fmt.Errorf("schema must be %q", VODRequestSchemaV2)
	}
	if !vodWorkloadIDPattern.MatchString(req.WorkloadID) {
		return transcode.Preset{}, errors.New("invalid workload_id")
	}
	if err := vodHTTPURLV2(req.Input.DownloadURL); err != nil {
		return transcode.Preset{}, errors.New("input.download_url must be http(s)")
	}
	if req.Input.ContentSHA256 != "" {
		if len(req.Input.ContentSHA256) != 64 {
			return transcode.Preset{}, errors.New("invalid content_sha256")
		}
		if _, err := hex.DecodeString(req.Input.ContentSHA256); err != nil {
			return transcode.Preset{}, errors.New("invalid content_sha256")
		}
	}
	if req.Rendition.Name == "" || req.Rendition.Width < 1 || req.Rendition.Height < 1 || req.Rendition.FPS < 1 {
		return transcode.Preset{}, errors.New("rendition name, width, height, and fps are required")
	}
	codec := strings.ToLower(req.Rendition.Codec)
	if codec == "" {
		codec = "h264"
	}
	if codec == "h265" {
		codec = "hevc"
	}
	if codec != "h264" && codec != "hevc" && codec != "av1" && codec != "vp9" {
		return transcode.Preset{}, errors.New("unsupported rendition codec")
	}
	if strings.TrimSpace(req.Output.Stream.ArtifactURI) == "" || strings.ContainsAny(req.Output.Stream.ArtifactURI, "?\r\n") {
		return transcode.Preset{}, errors.New("output.stream.artifact_uri must be a stable non-secret reference")
	}
	if err := vodHTTPURLV2(req.Output.Stream.UploadURL); err != nil {
		return transcode.Preset{}, errors.New("output.stream.upload_url must be http(s)")
	}
	var candidates []transcode.Preset
	for _, p := range s.presets {
		pc := strings.ToLower(p.VideoCodec)
		if pc == "h265" {
			pc = "hevc"
		}
		if pc == codec && p.Width == req.Rendition.Width && p.Height == req.Rendition.Height {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return transcode.Preset{}, errors.New("no runner preset supports requested codec and dimensions")
	}
	p := candidates[0]
	p.FPS = req.Rendition.FPS
	return p, nil
}

func vodHTTPURLV2(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return errors.New("invalid URL")
	}
	return nil
}
func vodRequestHashV2(req VODRequestV2) (string, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(req); err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	return hex.EncodeToString(sum[:]), nil
}
func vodFileHashV2(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func vodPreparedValidV2(root string, j vodJournalV2) bool {
	if j.PreparedPath == "" || filepath.IsAbs(j.PreparedPath) || strings.Contains(j.PreparedPath, "..") || j.PreparedHash == "" || j.Video == nil {
		return false
	}
	h, err := vodFileHashV2(filepath.Join(root, j.PreparedPath))
	return err == nil && h == j.PreparedHash
}
func vodUnitsV2(v *VODVideoV2) (uint64, error) {
	if v == nil {
		return 0, nil
	}
	hi, a := bits.Mul64(v.ActualFrames, uint64(v.Width))
	if hi != 0 {
		return 0, errors.New("usage overflow")
	}
	hi, p := bits.Mul64(a, uint64(v.Height))
	if hi != 0 {
		return 0, errors.New("usage overflow")
	}
	u := p / 1_000_000
	if p%1_000_000 != 0 {
		u++
	}
	return u, nil
}
func vodExactVideoV2(ctx context.Context, path string) (*VODVideoV2, error) {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-count_frames", "-select_streams", "v:0", "-show_entries", "stream=nb_read_frames,width,height", "-of", "json", path).Output()
	if err != nil {
		return nil, err
	}
	var p struct {
		Streams []struct {
			Frames string `json:"nb_read_frames"`
			Width  uint32 `json:"width"`
			Height uint32 `json:"height"`
		} `json:"streams"`
	}
	if json.Unmarshal(out, &p) != nil || len(p.Streams) == 0 {
		return nil, errors.New("missing video frame count")
	}
	frames, err := strconv.ParseUint(p.Streams[0].Frames, 10, 64)
	if err != nil || frames == 0 {
		return nil, errors.New("invalid video frame count")
	}
	return &VODVideoV2{ActualFrames: frames, Width: p.Streams[0].Width, Height: p.Streams[0].Height}, nil
}
func vodTerminalUnitsV2(j vodJournalV2) uint64 {
	if len(j.Events) == 0 {
		return 0
	}
	last := j.Events[len(j.Events)-1]
	if last.Event == "result" {
		var v VODResultV2
		if json.Unmarshal(last.Data, &v) == nil {
			return v.Usage.Units
		}
	}
	if last.Event == "error" {
		var v VODErrorV2
		if json.Unmarshal(last.Data, &v) == nil {
			return v.Usage.Units
		}
	}
	return 0
}

func handleRunnerContractV2(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	writeJSON(w, 200, map[string]any{"capability_id": "video:transcode.vod", "protocol": "paid-job/v1", "transports": []string{"stream"}, "work_unit": map[string]any{"name": VODWorkUnitV2, "extractor": map[string]any{"type": "response-trailer", "trailer": VODWorkUnitsTrailerV2}}, "paths": map[string]string{"invoke": "/v1/video/transcode"}, "readiness": map[string]string{"type": "http-status", "path": "/healthz"}, "identity": map[string]string{"provider": "transcode-runner"}, "schema_versions": map[string]string{"paid-job/v1": "1.0.15", VODRequestSchemaV2: "2.0.0"}})
}
