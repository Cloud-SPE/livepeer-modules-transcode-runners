package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

//go:embed presets.yaml
var defaultPresetsYAML []byte

var (
	runnerAddr   = env("RUNNER_ADDR", ":8080")
	maxQueueSize = envInt("MAX_QUEUE_SIZE", 5)
	stateDir     = env("STATE_DIR", "/var/lib/transcode-runner")
	hw           transcode.HWProfile
	presets      []transcode.Preset
	vodService   *VODServiceV2
	gpuAdmission *transcode.GPUAdmissionGate
)

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func guessExtension(raw string) string {
	if index := strings.IndexByte(raw, '?'); index >= 0 {
		raw = raw[:index]
	}
	if extension := filepath.Ext(raw); extension != "" {
		return extension
	}
	return ".mp4"
}

func scanFFmpegLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for index, value := range data {
		if value == '\n' || value == '\r' {
			return index + 1, data[:index], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func envInt(key string, fallback int) int {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return fallback
}

func handlePresets(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"presets": presets, "gpu": hw.GPUName, "gpu_vendor": string(hw.Vendor), "count": len(presets)})
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	active := int32(0)
	if vodService != nil {
		active = vodService.active.Load()
	}
	rejections := uint64(0)
	if vodService != nil {
		rejections = vodService.gpuAdmissionRejected.Load()
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "gpu": hw.GPUName, "vram_mb": hw.VRAM_MB, "active_jobs": active, "max_jobs": maxQueueSize, "presets": len(presets), "gpu_admission_configured": gpuAdmission != nil && gpuAdmission.Configured(), "gpu_admission_rejections": rejections})
}

func checkRunnerHealth(ctx context.Context, addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse runner address: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/healthz", nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("health endpoint returned %s", response.Status)
	}
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Print(transcode.BuildSummary("transcode-runner"))
	if len(os.Args) == 2 && os.Args[1] == "-healthcheck" {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if err := checkRunnerHealth(ctx, runnerAddr); err != nil {
			log.Printf("healthcheck failed: %v", err)
			os.Exit(1)
		}
		return
	}
	hw = transcode.DetectGPU()
	presetBody := defaultPresetsYAML
	if path := os.Getenv("PRESETS_FILE"); path != "" {
		body, err := os.ReadFile(path)
		if err != nil {
			log.Fatalf("read presets: %v", err)
		}
		presetBody = body
	}
	all, err := transcode.LoadPresetsFromBytes(presetBody)
	if err != nil {
		log.Fatalf("load presets: %v", err)
	}
	var skipped []string
	presets, skipped = transcode.ValidatePresets(all, hw)
	if len(presets) == 0 {
		log.Fatal("no presets are supported by this runner")
	}
	log.Printf("transcode-runner: %d active presets (%d skipped)", len(presets), len(skipped))
	store, err := NewVODStoreV2(stateDir)
	if err != nil {
		log.Fatalf("initialize workload state: %v", err)
	}
	gpuAdmission, err = transcode.NewGPUAdmissionGate(os.Getenv("GPU_ADMISSION_LOCK"))
	if err != nil {
		log.Fatalf("initialize GPU admission: %v", err)
	}
	vodService, err = NewVODServiceV2(store, presets, hw, maxQueueSize, gpuAdmission)
	if err != nil {
		log.Fatalf("initialize VOD service: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/video/transcode", vodService)
	mux.HandleFunc("/.well-known/livepeer-runner", handleRunnerContractV2)
	mux.HandleFunc("/v1/video/transcode/presets", handlePresets)
	mux.HandleFunc("/healthz", handleHealthz)
	server := &http.Server{Addr: runnerAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 0, IdleTimeout: 120 * time.Second}
	go func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
		<-signals
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	log.Printf("listening on %s (state_dir=%s)", runnerAddr, stateDir)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
