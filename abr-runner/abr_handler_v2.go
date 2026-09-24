package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

const maxABRRequestBodyV2 = 5 << 20

var errABRExecutionCapacityV2 = errors.New("ABR execution capacity reached")

// ABRExecutorV2 performs the paid exchange's work. Implementations must return
// only after all promised output is delivered or the context is canceled.
type ABRExecutorV2 interface {
	Execute(context.Context, ABRWorkloadRequestV2, transcode.ABRPreset, ABRExecutionReporterV2) (ABRTerminalResultV2, error)
}

type ABRExecutionReporterV2 interface {
	Progress(phase string, overallProgress float64, rendition string, frames uint64) error
	Prepared(PreparedRenditionV2) error
	Delivered(RenditionResultV2) error
	PreparedManifest(PreparedArtifactV2) error
	DeliveredManifest(string) error
}

// ABRExecutionErrorV2 is safe to persist and return over SSE. Underlying
// operational errors, especially errors containing signed URLs, stay local.
type ABRExecutionErrorV2 struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *ABRExecutionErrorV2) Error() string { return e.Code }

type abrExecutionV2 struct {
	requestSHA256 string
	cancel        context.CancelFunc
	subscribers   map[uint64]chan struct{}
	gpuLease      *transcode.GPUAdmissionLease
}

type ABRExecutionCoordinatorV2 struct {
	store        *FileWorkloadStoreV2
	executor     ABRExecutorV2
	limit        int
	gpuAdmission *transcode.GPUAdmissionGate

	mu                   sync.Mutex
	active               map[string]*abrExecutionV2
	nextSubscriber       uint64
	activeCount          atomic.Int32
	gpuAdmissionRejected atomic.Uint64
}

func NewABRExecutionCoordinatorV2(store *FileWorkloadStoreV2, executor ABRExecutorV2, limit int, admission *transcode.GPUAdmissionGate) (*ABRExecutionCoordinatorV2, error) {
	if store == nil || executor == nil || limit <= 0 {
		return nil, errors.New("store, executor, and positive execution limit are required")
	}
	return &ABRExecutionCoordinatorV2{
		store: store, executor: executor, limit: limit, gpuAdmission: admission, active: make(map[string]*abrExecutionV2),
	}, nil
}

func (c *ABRExecutionCoordinatorV2) Active() int32 { return c.activeCount.Load() }

type abrSubscriptionV2 struct {
	coordinator *ABRExecutionCoordinatorV2
	workloadID  string
	id          uint64
	notify      <-chan struct{}
	once        sync.Once
}

func (s *abrSubscriptionV2) Close() {
	s.once.Do(func() { s.coordinator.detach(s.workloadID, s.id) })
}

// Subscribe attaches to the one execution for this workload or starts it.
// Reloading the journal while holding the coordinator lock closes the race
// between a terminal write and removal of the active execution.
func (c *ABRExecutionCoordinatorV2) Subscribe(req ABRWorkloadRequestV2, requestSHA256 string, preset transcode.ABRPreset) (*abrSubscriptionV2, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	record, err := c.store.Load(req.WorkloadID)
	if err != nil {
		return nil, false, err
	}
	if record.RequestSHA256 != requestSHA256 {
		return nil, false, errors.New("request identity changed while subscribing")
	}
	if record.State != WorkloadInProgressV2 {
		return nil, true, nil
	}

	execution := c.active[req.WorkloadID]
	if execution == nil {
		if len(c.active) >= c.limit {
			return nil, false, errABRExecutionCapacityV2
		}
		lease, err := c.gpuAdmission.Acquire(transcode.GPUAdmissionBatch)
		if errors.Is(err, transcode.ErrGPUAdmissionCapacity) {
			c.gpuAdmissionRejected.Add(1)
			return nil, false, errABRExecutionCapacityV2
		}
		if err != nil {
			return nil, false, err
		}
		ctx, cancel := context.WithCancel(context.Background())
		execution = &abrExecutionV2{
			requestSHA256: requestSHA256,
			cancel:        cancel,
			subscribers:   make(map[uint64]chan struct{}),
			gpuLease:      lease,
		}
		c.active[req.WorkloadID] = execution
		c.activeCount.Add(1)
		go c.run(ctx, req, requestSHA256, preset)
	} else if execution.requestSHA256 != requestSHA256 {
		return nil, false, errors.New("active execution identity mismatch")
	}

	c.nextSubscriber++
	notify := make(chan struct{}, 1)
	execution.subscribers[c.nextSubscriber] = notify
	subscription := &abrSubscriptionV2{
		coordinator: c,
		workloadID:  req.WorkloadID,
		id:          c.nextSubscriber,
		notify:      notify,
	}
	return subscription, false, nil
}

func (c *ABRExecutionCoordinatorV2) detach(workloadID string, subscriberID uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	execution := c.active[workloadID]
	if execution == nil {
		return
	}
	delete(execution.subscribers, subscriberID)
	if len(execution.subscribers) == 0 {
		execution.cancel()
	}
}

func (c *ABRExecutionCoordinatorV2) run(ctx context.Context, req ABRWorkloadRequestV2, requestSHA256 string, preset transcode.ABRPreset) {
	reporter := &abrExecutionReporterV2{coordinator: c, workloadID: req.WorkloadID}
	result, executeErr := c.executor.Execute(ctx, req, preset, reporter)
	if ctx.Err() == nil {
		if executeErr == nil {
			_, executeErr = c.store.RecordTerminalResult(req.WorkloadID, result)
		} else {
			failure := safeABRExecutionFailureV2(executeErr)
			units, usageErr := c.deliveredWorkUnits(req.WorkloadID)
			if usageErr != nil {
				failure = ABRErrorV2{Code: "usage_failed", Message: "usage measurement failed", Retryable: false}
				units = 0
			}
			_, executeErr = c.store.RecordTerminalError(req.WorkloadID, ABRTerminalErrorV2{
				Schema:        ABRResultSchemaV2,
				WorkloadID:    req.WorkloadID,
				RequestSHA256: requestSHA256,
				Outcome:       "failed",
				Error:         failure,
				Usage:         UsageClaimV2{Unit: ABRWorkUnitV2, Units: units},
			})
		}
		if executeErr == nil {
			c.notify(req.WorkloadID)
		}
	}
	c.finish(req.WorkloadID)
}

func (c *ABRExecutionCoordinatorV2) deliveredWorkUnits(workloadID string) (uint64, error) {
	record, err := c.store.Load(workloadID)
	if err != nil {
		return 0, err
	}
	names := make([]string, 0, len(record.Delivered))
	for name := range record.Delivered {
		names = append(names, name)
	}
	sort.Strings(names)
	delivered := make([]RenditionResultV2, 0, len(names))
	for _, name := range names {
		delivered = append(delivered, record.Delivered[name])
	}
	return CalculateFrameMegapixelUnitsV2(delivered)
}

func safeABRExecutionFailureV2(err error) ABRErrorV2 {
	var executionError *ABRExecutionErrorV2
	if errors.As(err, &executionError) {
		candidate := ABRErrorV2{Code: executionError.Code, Message: executionError.Message, Retryable: executionError.Retryable}
		probe := ABRTerminalErrorV2{
			Schema: ABRResultSchemaV2, WorkloadID: "validation", RequestSHA256: strings.Repeat("0", 64),
			Outcome: "failed", Error: candidate, Usage: UsageClaimV2{Unit: ABRWorkUnitV2, Units: 0},
		}
		if ValidateABRTerminalErrorV2(probe) == nil {
			return candidate
		}
	}
	return ABRErrorV2{Code: "execution_failed", Message: "ABR execution failed", Retryable: true}
}

func (c *ABRExecutionCoordinatorV2) notify(workloadID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, subscriber := range c.active[workloadID].subscribers {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
}

func (c *ABRExecutionCoordinatorV2) finish(workloadID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	execution := c.active[workloadID]
	if execution == nil {
		return
	}
	delete(c.active, workloadID)
	c.activeCount.Add(-1)
	_ = execution.gpuLease.Close()
	for _, subscriber := range execution.subscribers {
		close(subscriber)
	}
}

type abrExecutionReporterV2 struct {
	coordinator *ABRExecutionCoordinatorV2
	workloadID  string
}

func (r *abrExecutionReporterV2) Progress(phase string, overallProgress float64, rendition string, frames uint64) error {
	_, err := r.coordinator.store.AppendProgress(r.workloadID, phase, overallProgress, rendition, frames)
	if err == nil {
		r.coordinator.notify(r.workloadID)
	}
	return err
}

func (r *abrExecutionReporterV2) Prepared(prepared PreparedRenditionV2) error {
	return r.coordinator.store.SavePrepared(r.workloadID, prepared)
}

func (r *abrExecutionReporterV2) Delivered(delivered RenditionResultV2) error {
	return r.coordinator.store.SaveDelivered(r.workloadID, delivered)
}

func (r *abrExecutionReporterV2) PreparedManifest(prepared PreparedArtifactV2) error {
	return r.coordinator.store.SavePreparedManifest(r.workloadID, prepared)
}

func (r *abrExecutionReporterV2) DeliveredManifest(artifactURI string) error {
	return r.coordinator.store.SaveDeliveredManifest(r.workloadID, artifactURI)
}

type ABRHandlerV2 struct {
	store       *FileWorkloadStoreV2
	coordinator *ABRExecutionCoordinatorV2
	presets     []transcode.ABRPreset
	keepalive   time.Duration
}

func NewABRHandlerV2(store *FileWorkloadStoreV2, coordinator *ABRExecutionCoordinatorV2, presets []transcode.ABRPreset) (*ABRHandlerV2, error) {
	if store == nil || coordinator == nil || len(presets) == 0 {
		return nil, errors.New("store, coordinator, and presets are required")
	}
	return &ABRHandlerV2{store: store, coordinator: coordinator, presets: presets, keepalive: 5 * time.Second}, nil
}

func (h *ABRHandlerV2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		writeJSON(w, http.StatusNotAcceptable, map[string]string{"error": "sse_transport_required"})
		return
	}

	var req ABRWorkloadRequestV2
	if err := decodeABRRequestV2(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "message": err.Error()})
		return
	}
	preset, ok := transcode.FindABRPreset(h.presets, req.Ladder.Preset)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown_preset"})
		return
	}
	if err := ValidateABRRequestV2(req, preset.RenditionNames()); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "message": err.Error()})
		return
	}
	requestSHA256, err := RequestContentSHA256V2(req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "request_hash_failed"})
		return
	}
	record, disposition, err := h.store.LoadOrCreate(req.WorkloadID, requestSHA256)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "journal_unavailable"})
		return
	}
	if disposition == RejectIDReuseV2 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "workload_id_reuse"})
		return
	}

	var subscription *abrSubscriptionV2
	if disposition != ReplayTerminalV2 {
		var becameTerminal bool
		subscription, becameTerminal, err = h.coordinator.Subscribe(req, requestSHA256, preset)
		if errors.Is(err, errABRExecutionCapacityV2) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "capacity_reached"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "execution_unavailable"})
			return
		}
		if becameTerminal {
			record, err = h.store.Load(req.WorkloadID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "journal_unavailable"})
				return
			}
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Add("Trailer", ABRWorkUnitsTrailerV2)
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	flusher.Flush()
	if subscription != nil {
		defer subscription.Close()
	}

	lastSequence, terminal, err := writeJournalEventsV2(w, flusher, record, 0)
	if err != nil {
		return
	}
	if terminal {
		writeWorkUnitsTrailerV2(w, record)
		return
	}
	if subscription == nil {
		return
	}

	ticker := time.NewTicker(h.keepalive)
	defer ticker.Stop()
	for {
		select {
		case _, open := <-subscription.notify:
			record, err = h.store.Load(req.WorkloadID)
			if err != nil {
				return
			}
			lastSequence, terminal, err = writeJournalEventsV2(w, flusher, record, lastSequence)
			if err != nil {
				return
			}
			if terminal {
				writeWorkUnitsTrailerV2(w, record)
				return
			}
			if !open {
				return
			}
		case <-ticker.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func writeWorkUnitsTrailerV2(w http.ResponseWriter, record WorkloadJournalV2) {
	units, err := terminalWorkUnitsV2(record)
	if err != nil {
		return
	}
	w.Header().Set(ABRWorkUnitsTrailerV2, strconv.FormatUint(units, 10))
}

func terminalWorkUnitsV2(record WorkloadJournalV2) (uint64, error) {
	if record.TerminalEvent == nil {
		return 0, errors.New("terminal event is absent")
	}
	switch record.TerminalEvent.Event {
	case "result":
		var result ABRTerminalResultV2
		if err := json.Unmarshal(record.TerminalEvent.Data, &result); err != nil {
			return 0, err
		}
		return result.Usage.Units, nil
	case "error":
		var result ABRTerminalErrorV2
		if err := json.Unmarshal(record.TerminalEvent.Data, &result); err != nil {
			return 0, err
		}
		return result.Usage.Units, nil
	default:
		return 0, fmt.Errorf("unsupported terminal event %q", record.TerminalEvent.Event)
	}
}

func decodeABRRequestV2(w http.ResponseWriter, r *http.Request, value *ABRWorkloadRequestV2) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxABRRequestBodyV2)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body contains multiple JSON values")
		}
		return err
	}
	return nil
}

func writeJournalEventsV2(w io.Writer, flusher http.Flusher, record WorkloadJournalV2, after uint64) (uint64, bool, error) {
	last := after
	for _, event := range record.ProgressEvents {
		if event.Sequence <= after {
			continue
		}
		if err := writeSSEEventV2(w, event); err != nil {
			return last, false, err
		}
		last = event.Sequence
		flusher.Flush()
	}
	if record.TerminalEvent != nil && record.TerminalEvent.Sequence > after {
		if err := writeSSEEventV2(w, *record.TerminalEvent); err != nil {
			return last, false, err
		}
		flusher.Flush()
		return record.TerminalEvent.Sequence, true, nil
	}
	return last, record.TerminalEvent != nil, nil
}

func writeSSEEventV2(w io.Writer, event StoredSSEEventV2) error {
	_, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Sequence, event.Event, event.Data)
	return err
}
