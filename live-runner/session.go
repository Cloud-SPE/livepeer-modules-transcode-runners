package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

type sessionRuntime struct {
	mu              sync.Mutex
	rec             sessionRecord
	cfg             config
	callbacks       *callbackClient
	metrics         *runnerMetrics
	ffmpegCancel    context.CancelFunc
	ffmpegDone      chan struct{}
	uploadCancel    context.CancelFunc
	uploadDone      chan struct{}
	sequence        atomic.Uint64
	lastUsageTotal  atomic.Uint64
	lastProgressAt  atomic.Int64
	lastHeartbeatAt atomic.Int64
	lastEventTime   atomic.Int64
	terminating     atomic.Bool
}

func newSessionRuntime(cfg config, req liveSessionRequest, preset transcodePreset, port int, callbacks *callbackClient, metrics *runnerMetrics) (*sessionRuntime, error) {
	sessionID, err := randomID("rsess_")
	if err != nil {
		return nil, err
	}
	streamKey, err := randomStreamKey()
	if err != nil {
		return nil, err
	}

	mode := modeLocalHLSServe
	if req.OutputCredential != nil {
		mode = modeGatewayIngest
		if req.IngestAccept == nil || strings.TrimSpace(req.IngestAccept.StreamKey) == "" {
			return nil, errors.New("ingest_accept.stream_key is required when output_credential is present")
		}
		streamKey = strings.TrimSpace(req.IngestAccept.StreamKey)
	}

	rec := sessionRecord{
		RunnerSessionID:  sessionID,
		BrokerSessionID:  req.BrokerSessionID,
		WorkID:           req.WorkID,
		CapabilityID:     req.CapabilityID,
		OfferingID:       req.OfferingID,
		Name:             req.SessionParams.Name,
		State:            stateProvisioning,
		Mode:             mode,
		RTMPPort:         port,
		CreatedAt:        time.Now().UTC(),
		StreamKey:        streamKey,
		Callbacks:        req.BrokerCallbacks,
		IdleTimeout:      cfg.SessionIdleTTL,
		Preset:           preset.ABRPreset,
		OutputCredential: req.OutputCredential,
		Ingest: liveIngestStatus{
			Mode:            mode,
			StreamKeySuffix: maskSecretSuffix(streamKey),
		},
		Output: liveOutputStatus{
			Mode: outputModeLocalHLS,
		},
	}
	if mode == modeGatewayIngest {
		rec.Output.Mode = outputModeS3Push
		rec.Output.TargetPrefix = req.OutputCredential.KeyPrefix
	}
	if req.SessionParams.IdleTimeoutSeconds > 0 {
		rec.IdleTimeout = time.Duration(req.SessionParams.IdleTimeoutSeconds) * time.Second
	}
	rec.RTMPURL = "rtmp://" + cfg.RTMPHost + ":" + itoa(port) + "/live"
	rec.PrivateIngestURL = "rtmp://" + cfg.IngestPublicHost + ":" + itoa(port) + "/live/" + streamKey
	rec.OutputDir = cfg.TempDir + "/" + sessionID
	if mode == modeLocalHLSServe {
		rec.HLSURL = cfg.HLSBasePath + "/" + sessionID + "/master.m3u8"
	}

	return &sessionRuntime{
		rec:       rec,
		cfg:       cfg,
		callbacks: callbacks,
		metrics:   metrics,
	}, nil
}

func (rt *sessionRuntime) setState(next sessionState, reason string) {
	rt.rec.State = next
	if reason != "" {
		rt.rec.CloseReason = reason
	}
	if next == stateEnded || next == stateFailed {
		rt.rec.EndedAt = time.Now().UTC()
		rt.rec.Terminated = true
	}
}

func (rt *sessionRuntime) markProgress(total uint64) (delta uint64, started bool) {
	now := time.Now().UTC()
	rt.lastProgressAt.Store(now.UnixNano())
	rt.mu.Lock()
	defer rt.mu.Unlock()
	previous := rt.lastUsageTotal.Load()
	if total > previous {
		delta = total - previous
		rt.lastUsageTotal.Store(total)
	}
	if rt.rec.State == stateReady || rt.rec.State == stateProvisioning || rt.rec.State == stateStalled {
		rt.rec.StartedAt = now
		rt.rec.LastPacketAt = now
		rt.rec.Ingest.Authenticated = true
		rt.rec.Ingest.ConnectedPublisher = true
		rt.rec.Ingest.LastPacketAt = now
		rt.setState(statePublishing, "")
		started = true
	} else if rt.rec.State == statePublishing {
		rt.rec.LastPacketAt = now
		rt.rec.Ingest.ConnectedPublisher = true
		rt.rec.Ingest.LastPacketAt = now
	}
	return delta, started
}

func (rt *sessionRuntime) heartbeatNow() time.Time {
	now := time.Now().UTC()
	rt.lastHeartbeatAt.Store(now.UnixNano())
	rt.mu.Lock()
	if rt.rec.State == statePublishing || rt.rec.State == stateStalled {
		rt.rec.LastPacketAt = now
		rt.rec.Ingest.LastPacketAt = now
	}
	rt.mu.Unlock()
	return now
}

func (rt *sessionRuntime) query() getSessionResponse {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	resp := getSessionResponse{
		RunnerSessionID: rt.rec.RunnerSessionID,
		BrokerSessionID: rt.rec.BrokerSessionID,
		State:           rt.rec.State,
		Ingest: liveIngestStatusResponse{
			Mode:               rt.rec.Ingest.Mode,
			ListenerBound:      rt.rec.Ingest.ListenerBound,
			Authenticated:      rt.rec.Ingest.Authenticated,
			ConnectedPublisher: rt.rec.Ingest.ConnectedPublisher,
			StreamKeySuffix:    rt.rec.Ingest.StreamKeySuffix,
		},
		Output: liveOutputStatusResponse{
			Mode:            rt.rec.Output.Mode,
			TargetPrefix:    rt.rec.Output.TargetPrefix,
			PutSuccessCount: rt.rec.Output.PutSuccessCount,
			PutFailureCount: rt.rec.Output.PutFailureCount,
		},
		UsageTotal: rt.lastUsageTotal.Load(),
		UsageUnit:  "output_seconds",
	}
	if !rt.rec.StartedAt.IsZero() {
		v := rt.rec.StartedAt.Format(time.RFC3339)
		resp.StartedAt = &v
	}
	if !rt.rec.LastPacketAt.IsZero() {
		v := rt.rec.LastPacketAt.Format(time.RFC3339)
		resp.LastPacketAt = &v
		resp.Ingest.LastPacketAt = &v
	}
	if ts := rt.lastHeartbeatAt.Load(); ts > 0 {
		v := time.Unix(0, ts).UTC().Format(time.RFC3339)
		resp.LastHeartbeatAt = &v
	}
	if rt.rec.CloseReason != "" {
		v := rt.rec.CloseReason
		resp.CloseReason = &v
	}
	if !rt.rec.EndedAt.IsZero() {
		v := rt.rec.EndedAt.Format(time.RFC3339)
		resp.EndedAt = &v
	}
	if !rt.rec.Output.LastManifestPutAt.IsZero() {
		v := rt.rec.Output.LastManifestPutAt.Format(time.RFC3339)
		resp.Output.LastManifestPutAt = &v
	}
	if !rt.rec.Output.LastSegmentPutAt.IsZero() {
		v := rt.rec.Output.LastSegmentPutAt.Format(time.RFC3339)
		resp.Output.LastSegmentPutAt = &v
	}
	if rt.rec.Output.LastPutError != "" {
		v := rt.rec.Output.LastPutError
		resp.Output.LastPutError = &v
	}
	return resp
}

func (rt *sessionRuntime) beginTerminate(reason string) bool {
	if !rt.terminating.CompareAndSwap(false, true) {
		return false
	}
	rt.mu.Lock()
	if rt.rec.State != stateEnded && rt.rec.State != stateFailed {
		rt.setState(stateEnding, reason)
	}
	rt.mu.Unlock()
	return true
}

func (rt *sessionRuntime) finishTerminate(reason string, failed bool) {
	rt.mu.Lock()
	if failed {
		rt.setState(stateFailed, reason)
	} else {
		rt.setState(stateEnded, reason)
	}
	rt.mu.Unlock()
}

func (rt *sessionRuntime) stop(reason string) {
	if !rt.beginTerminate(reason) {
		return
	}
	if rt.ffmpegCancel != nil {
		rt.ffmpegCancel()
	}
	if rt.uploadCancel != nil {
		rt.uploadCancel()
	}
	if rt.ffmpegDone != nil {
		<-rt.ffmpegDone
	}
	if rt.uploadDone != nil {
		<-rt.uploadDone
	}
	rt.finishTerminate(reason, false)
}

func (rt *sessionRuntime) fail(reason string) {
	if !rt.beginTerminate(reason) {
		return
	}
	if rt.ffmpegCancel != nil {
		rt.ffmpegCancel()
	}
	if rt.uploadCancel != nil {
		rt.uploadCancel()
	}
	if rt.ffmpegDone != nil {
		<-rt.ffmpegDone
	}
	if rt.uploadDone != nil {
		<-rt.uploadDone
	}
	rt.finishTerminate(reason, true)
}

func (rt *sessionRuntime) event(eventType string, usageTotal, usageDelta uint64, closeReason string, details map[string]any) {
	if rt.callbacks == nil || rt.callbacks.disabled() {
		return
	}
	seq := rt.sequence.Add(1)
	env := eventEnvelope{
		BrokerSessionID: rt.rec.BrokerSessionID,
		RunnerSessionID: rt.rec.RunnerSessionID,
		EventID:         eventID(eventType, rt.rec.RunnerSessionID, seq),
		Sequence:        seq,
		EventType:       eventType,
		EventTime:       time.Now().UTC().Format(time.RFC3339),
		State:           rt.rec.State,
		Usage: eventUsage{
			Unit:  "output_seconds",
			Delta: usageDelta,
			Total: usageTotal,
		},
		Details: details,
	}
	if closeReason != "" {
		env.CloseReason = &closeReason
	}
	rt.callbacks.send(context.Background(), rt.rec.Callbacks, env)
}

func eventID(kind, sessionID string, seq uint64) string {
	return kind + "_" + sessionID + "_" + itoa64(seq)
}

func randomID(prefix string) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

func randomStreamKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "lvk_" + base64.RawURLEncoding.EncodeToString(b), nil
}

type transcodePreset struct {
	transcode.ABRPreset
}

func (rt *sessionRuntime) watchdog(noPublishTTL time.Duration) error {
	now := time.Now().UTC()
	rt.mu.Lock()
	state := rt.rec.State
	createdAt := rt.rec.CreatedAt
	lastPacketAt := rt.rec.LastPacketAt
	idleTimeout := rt.rec.IdleTimeout
	rt.mu.Unlock()

	switch state {
	case stateReady:
		if noPublishTTL > 0 && now.Sub(createdAt) > noPublishTTL {
			log.Printf("[live %s] no publish timeout", rt.rec.RunnerSessionID)
			rt.fail("no_publish_timeout")
			return errors.New("no publish timeout")
		}
	case statePublishing:
		if idleTimeout > 0 && !lastPacketAt.IsZero() && now.Sub(lastPacketAt) > idleTimeout {
			log.Printf("[live %s] idle timeout", rt.rec.RunnerSessionID)
			rt.mu.Lock()
			rt.rec.Ingest.ConnectedPublisher = false
			rt.setState(stateStalled, "idle_timeout")
			rt.mu.Unlock()
			return errors.New("idle timeout")
		}
	case stateStalled:
		if idleTimeout > 0 && !lastPacketAt.IsZero() && now.Sub(lastPacketAt) > idleTimeout*2 {
			log.Printf("[live %s] stalled session failed", rt.rec.RunnerSessionID)
			rt.fail("idle_timeout")
			return errors.New("idle timeout")
		}
	}
	return nil
}

func (rt *sessionRuntime) setListenerBound(bound bool) {
	rt.mu.Lock()
	rt.rec.Ingest.ListenerBound = bound
	rt.mu.Unlock()
}

func (rt *sessionRuntime) recordOutputPut(name string, when time.Time) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.rec.Output.PutSuccessCount++
	if rt.metrics != nil {
		rt.metrics.outputCredentialPutSuccess.Add(1)
	}
	rt.rec.Output.LastPutError = ""
	switch {
	case strings.HasSuffix(name, ".m3u8"):
		rt.rec.Output.LastManifestPutAt = when
	default:
		rt.rec.Output.LastSegmentPutAt = when
	}
}

func (rt *sessionRuntime) recordOutputError(err error) {
	if err == nil {
		return
	}
	rt.mu.Lock()
	rt.rec.Output.PutFailureCount++
	if rt.metrics != nil {
		rt.metrics.outputCredentialPutFailure.Add(1)
	}
	rt.rec.Output.LastPutError = err.Error()
	rt.mu.Unlock()
}

func maskSecretSuffix(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}
