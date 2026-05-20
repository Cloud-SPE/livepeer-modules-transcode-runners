package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
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
	ffmpegCancel    context.CancelFunc
	ffmpegDone      chan struct{}
	sequence        atomic.Uint64
	lastUsageTotal  atomic.Uint64
	lastProgressAt  atomic.Int64
	lastHeartbeatAt atomic.Int64
	lastEventTime   atomic.Int64
	started         atomic.Bool
	terminating     atomic.Bool
}

func newSessionRuntime(cfg config, req liveSessionRequest, preset transcodePreset, port int, callbacks *callbackClient) (*sessionRuntime, error) {
	sessionID, err := randomID("rsess_")
	if err != nil {
		return nil, err
	}
	streamKey, err := randomStreamKey()
	if err != nil {
		return nil, err
	}

	rec := sessionRecord{
		RunnerSessionID: sessionID,
		BrokerSessionID: req.BrokerSessionID,
		WorkID:          req.WorkID,
		CapabilityID:    req.CapabilityID,
		OfferingID:      req.OfferingID,
		Name:            req.SessionParams.Name,
		State:           stateProvisioning,
		RTMPPort:        port,
		CreatedAt:       time.Now().UTC(),
		StreamKey:       streamKey,
		Callbacks:       req.BrokerCallbacks,
		IdleTimeout:     cfg.SessionIdleTTL,
		Preset:          preset.ABRPreset,
	}
	if req.SessionParams.IdleTimeoutSeconds > 0 {
		rec.IdleTimeout = time.Duration(req.SessionParams.IdleTimeoutSeconds) * time.Second
	}
	rec.RTMPURL = "rtmp://" + cfg.RTMPHost + ":" + itoa(port) + "/live"
	rec.OutputDir = cfg.TempDir + "/" + sessionID
	rec.HLSURL = cfg.HLSBasePath + "/" + sessionID + "/master.m3u8"

	rt := &sessionRuntime{
		rec:       rec,
		cfg:       cfg,
		callbacks: callbacks,
	}
	return rt, nil
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
	if rt.rec.State == stateReady || rt.rec.State == stateProvisioning {
		rt.rec.StartedAt = now
		rt.rec.LastPacketAt = now
		rt.setState(statePublishing, "")
		started = true
	} else if rt.rec.State == statePublishing {
		rt.rec.LastPacketAt = now
	}
	return delta, started
}

func (rt *sessionRuntime) heartbeatNow() time.Time {
	now := time.Now().UTC()
	rt.lastHeartbeatAt.Store(now.UnixNano())
	rt.mu.Lock()
	if rt.rec.State == statePublishing {
		rt.rec.LastPacketAt = now
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
		UsageTotal:      rt.lastUsageTotal.Load(),
		UsageUnit:       "output_seconds",
	}
	if !rt.rec.StartedAt.IsZero() {
		v := rt.rec.StartedAt.Format(time.RFC3339)
		resp.StartedAt = &v
	}
	if !rt.rec.LastPacketAt.IsZero() {
		v := rt.rec.LastPacketAt.Format(time.RFC3339)
		resp.LastPacketAt = &v
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
	if rt.ffmpegDone != nil {
		<-rt.ffmpegDone
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
	if rt.ffmpegDone != nil {
		<-rt.ffmpegDone
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
			rt.fail("idle_timeout")
			return errors.New("idle timeout")
		}
	}
	return nil
}
