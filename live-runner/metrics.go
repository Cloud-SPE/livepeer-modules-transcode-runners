package liverunner

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// LiveRunnerMetricsV1 is deliberately bounded: HTTP status is a finite label
// domain, and callback bodies never become metric labels.
type LiveRunnerMetricsV1 struct {
	callbackRejected     [600]atomic.Uint64
	ladderStarts         [7]atomic.Uint64
	ladderExits          [6]atomic.Uint64
	sessionsStalled      atomic.Uint64
	gpuProbes            [3]atomic.Uint64
	gpuAdmissionRejected atomic.Uint64
}

var ladderMetricCodesV1 = [...]string{"encoder_init_failed", "hwaccel_init_failed", "input_unavailable", "output_rejected", "process_killed", "unknown", "started"}
var gpuProbeResultsV1 = [...]string{"available", "unavailable", "unsupported"}

func (m *LiveRunnerMetricsV1) RecordCallbackRejected(statusCode int, _ string) {
	if m == nil || statusCode < 100 || statusCode >= len(m.callbackRejected) {
		return
	}
	m.callbackRejected[statusCode].Add(1)
}

func (m *LiveRunnerMetricsV1) RecordLadderStart(code string) {
	if m != nil {
		if index := ladderMetricCodeIndexV1(code); index >= 0 {
			m.ladderStarts[index].Add(1)
		}
	}
}

func (m *LiveRunnerMetricsV1) RecordLadderExit(code string) {
	if m != nil {
		if index := ladderMetricCodeIndexV1(code); index >= 0 && index < len(m.ladderExits) {
			m.ladderExits[index].Add(1)
		}
	}
}

func (m *LiveRunnerMetricsV1) RecordSessionStalled() {
	if m != nil {
		m.sessionsStalled.Add(1)
	}
}

func (m *LiveRunnerMetricsV1) RecordGPUProbe(result string) {
	if m == nil {
		return
	}
	for index, candidate := range gpuProbeResultsV1 {
		if result == candidate {
			m.gpuProbes[index].Add(1)
			return
		}
	}
}

func (m *LiveRunnerMetricsV1) RecordGPUAdmissionRejected() {
	if m != nil {
		m.gpuAdmissionRejected.Add(1)
	}
}

func (m *LiveRunnerMetricsV1) ServeHTTP(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintln(writer, "# HELP live_callback_rejected_total Permanently rejected live runner callbacks.")
	_, _ = fmt.Fprintln(writer, "# TYPE live_callback_rejected_total counter")
	for statusCode := 100; statusCode < len(m.callbackRejected); statusCode++ {
		if count := m.callbackRejected[statusCode].Load(); count > 0 {
			_, _ = fmt.Fprintf(writer, "live_callback_rejected_total{status=\"%d\"} %d\n", statusCode, count)
		}
	}
	_, _ = fmt.Fprintln(writer, "# HELP live_ladder_starts_total Live ladder process start attempts by safe result code.")
	_, _ = fmt.Fprintln(writer, "# TYPE live_ladder_starts_total counter")
	for index, code := range ladderMetricCodesV1 {
		if count := m.ladderStarts[index].Load(); count > 0 {
			_, _ = fmt.Fprintf(writer, "live_ladder_starts_total{code=\"%s\"} %d\n", code, count)
		}
	}
	_, _ = fmt.Fprintln(writer, "# HELP live_ladder_exits_total Live ladder process exits by safe code.")
	_, _ = fmt.Fprintln(writer, "# TYPE live_ladder_exits_total counter")
	for index, code := range ladderMetricCodesV1[:len(m.ladderExits)] {
		if count := m.ladderExits[index].Load(); count > 0 {
			_, _ = fmt.Fprintf(writer, "live_ladder_exits_total{code=\"%s\"} %d\n", code, count)
		}
	}
	_, _ = fmt.Fprintln(writer, "# HELP live_sessions_stalled_total Live sessions entering stalled output state.")
	_, _ = fmt.Fprintln(writer, "# TYPE live_sessions_stalled_total counter")
	_, _ = fmt.Fprintf(writer, "live_sessions_stalled_total %d\n", m.sessionsStalled.Load())
	_, _ = fmt.Fprintln(writer, "# HELP live_gpu_telemetry_probes_total Live ladder GPU pressure probe attempts by result.")
	_, _ = fmt.Fprintln(writer, "# TYPE live_gpu_telemetry_probes_total counter")
	for index, result := range gpuProbeResultsV1 {
		if count := m.gpuProbes[index].Load(); count > 0 {
			_, _ = fmt.Fprintf(writer, "live_gpu_telemetry_probes_total{result=\"%s\"} %d\n", result, count)
		}
	}
	_, _ = fmt.Fprintln(writer, "# HELP live_gpu_admission_rejected_total Live sessions rejected by local or shared GPU admission.")
	_, _ = fmt.Fprintln(writer, "# TYPE live_gpu_admission_rejected_total counter")
	_, _ = fmt.Fprintf(writer, "live_gpu_admission_rejected_total %d\n", m.gpuAdmissionRejected.Load())
}

func ladderMetricCodeIndexV1(code string) int {
	for index, candidate := range ladderMetricCodesV1 {
		if code == candidate {
			return index
		}
	}
	return -1
}
