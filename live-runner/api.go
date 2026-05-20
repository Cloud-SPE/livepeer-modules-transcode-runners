package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

//go:embed presets.yaml
var defaultPresetsYAML []byte

type server struct {
	cfg       config
	store     *sessionStore
	callbacks *callbackClient
	hw        transcode.HWProfile
	presets   []transcode.ABRPreset
}

func newServer(cfg config) (*server, error) {
	presetBytes := defaultPresetsYAML
	if cfg.PresetsFile != "" {
		data, err := os.ReadFile(cfg.PresetsFile)
		if err != nil {
			return nil, err
		}
		presetBytes = data
	}
	presets, err := transcode.LoadABRPresetsFromBytes(presetBytes)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.TempDir, 0o755); err != nil {
		return nil, err
	}
	return &server{
		cfg:       cfg,
		store:     newSessionStore(),
		callbacks: newCallbackClient(cfg.CallbackTimeout),
		hw:        transcode.DetectGPU(),
		presets:   presets,
	}, nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("GET "+s.cfg.HLSBasePath+"/", hlsHandler(s.cfg.TempDir, s.cfg.HLSBasePath))
	mux.HandleFunc("POST /v1/video/live/sessions", s.auth(s.handleCreate))
	mux.HandleFunc("GET /v1/video/live/sessions/{id}", s.auth(s.handleGet))
	mux.HandleFunc("DELETE /v1/video/live/sessions/{id}", s.auth(s.handleDelete))
	return mux
}

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.BrokerToken == "" {
			next(w, r)
			return
		}
		if auth := strings.TrimSpace(r.Header.Get("Authorization")); auth == "Bearer "+s.cfg.BrokerToken {
			next(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	}
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"build":    transcode.BuildSummary("live-runner"),
		"gpu":      s.hw.GPUName,
		"sessions": len(s.store.snapshot()),
	})
}

func (s *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req liveSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	preset, err := s.resolvePreset(req.SessionParams)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	port, err := s.store.nextPort(s.cfg.RTMPPortStart, s.cfg.RTMPPortEnd)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	rt, err := newSessionRuntime(s.cfg, req, preset, port, s.callbacks)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	plan, err := buildRuntimePlan(s.cfg, rt.rec, s.hw)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := s.store.add(rt); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if err := startFFmpeg(rt, plan, s.hw); err != nil {
		s.store.delete(rt.rec.RunnerSessionID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	rt.mu.Lock()
	rt.setState(stateReady, "")
	rt.mu.Unlock()
	resp := createSessionResponse{
		RunnerSessionID: rt.rec.RunnerSessionID,
		State:           rt.rec.State,
		CreatedAt:       rt.rec.CreatedAt.Format(time.RFC3339),
	}
	resp.Media.Ingest.RTMPURL = rt.rec.RTMPURL
	resp.Media.Ingest.StreamKey = rt.rec.StreamKey
	resp.Media.Playback.HLSURL = rt.rec.HLSURL
	writeJSON(w, http.StatusCreated, resp)
}

func (s *server) handleGet(w http.ResponseWriter, r *http.Request) {
	rt := s.store.get(r.PathValue("id"))
	if rt == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	writeJSON(w, http.StatusOK, rt.query())
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	rt := s.store.get(r.PathValue("id"))
	if rt == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	var req deleteSessionRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "broker_close"
	}
	rt.stop(reason)
	rt.event("session.ended", rt.lastUsageTotal.Load(), 0, reason, nil)
	writeJSON(w, http.StatusOK, deleteSessionResponse{
		RunnerSessionID: rt.rec.RunnerSessionID,
		State:           rt.rec.State,
		CloseReason:     rt.rec.CloseReason,
		EndedAt:         rt.rec.EndedAt.Format(time.RFC3339),
	})
}

func (s *server) resolvePreset(params liveSessionParams) (transcodePreset, error) {
	if params.Ladder != nil && len(params.Ladder.Rungs) > 0 {
		return transcodePreset{ABRPreset: inlinePreset(params)}, nil
	}
	name := params.Preset
	if name == "" {
		name = s.cfg.DefaultPreset
	}
	p, ok := transcode.FindABRPreset(s.presets, name)
	if !ok {
		return transcodePreset{}, errors.New("unknown live preset: " + name)
	}
	return transcodePreset{ABRPreset: p}, nil
}

func inlinePreset(params liveSessionParams) transcode.ABRPreset {
	renditions := make([]transcode.ABRRendition, 0, len(params.Ladder.Rungs))
	for _, rung := range params.Ladder.Rungs {
		bitrate := "128k"
		video := &transcode.ABRVideoSettings{
			Codec:      "h264",
			Width:      rung.Width,
			Height:     rung.Height,
			Bitrate:    itoa(max(1, rung.BitrateKbps)) + "k",
			MaxBitrate: itoa(max(1, rung.BitrateKbps*11/10)) + "k",
			BufSize:    itoa(max(1, rung.BitrateKbps*15/10)) + "k",
			Profile:    "main",
			Level:      "4.0",
			PixFmt:     "yuv420p",
		}
		if rung.Passthrough && rung.Width == 0 && rung.Height == 0 {
			video.Width = 1280
			video.Height = 720
		}
		renditions = append(renditions, transcode.ABRRendition{
			Name:  sanitizeName(rung.Name),
			Video: video,
			Audio: transcode.ABRAudioSettings{Codec: "aac", Bitrate: bitrate, Channels: 2, SampleRate: 48000},
		})
	}
	return transcode.ABRPreset{
		Name:            "inline",
		Description:     "inline live ladder",
		Type:            "live",
		Format:          "hls",
		HLSMode:         "event",
		SegmentDuration: 4,
		Renditions:      renditions,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) run(ctx context.Context) error {
	httpSrv := &http.Server{
		Addr:    s.cfg.RunnerAddr,
		Handler: s.routes(),
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, rt := range s.store.snapshot() {
					if err := rt.watchdog(s.cfg.SessionNoPublishTTL); err != nil {
						log.Printf("[live %s] watchdog: %v", rt.rec.RunnerSessionID, err)
						rt.event("session.failed", rt.lastUsageTotal.Load(), 0, rt.rec.CloseReason, map[string]any{"reason": rt.rec.CloseReason})
					} else if rt.rec.State == statePublishing {
						rt.heartbeatNow()
						rt.event("session.heartbeat", rt.lastUsageTotal.Load(), 0, "", nil)
					}
				}
				s.store.reap()
			}
		}
	}()

	go func() {
		<-ctx.Done()
		_ = httpSrv.Shutdown(context.Background())
		for _, rt := range s.store.snapshot() {
			rt.stop("runner_shutdown")
		}
	}()

	log.Printf("%s", transcode.BuildSummary("live-runner"))
	log.Printf("live runner listening on %s", s.cfg.RunnerAddr)
	return httpSrv.ListenAndServe()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func joinURLPath(base string, parts ...string) string {
	all := append([]string{base}, parts...)
	return filepath.ToSlash(filepath.Join(all...))
}
