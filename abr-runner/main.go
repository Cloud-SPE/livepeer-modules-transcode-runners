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
	runnerAddr       = env("RUNNER_ADDR", ":8080")
	maxQueueSize     = envInt("MAX_QUEUE_SIZE", 2)
	stateDir         = env("STATE_DIR", "/var/lib/abr-runner")
	hw               transcode.HWProfile
	abrPresets       []transcode.ABRPreset
	abrCoordinatorV2 *ABRExecutionCoordinatorV2
	gpuAdmission     *transcode.GPUAdmissionGate
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
	writeJSON(w, http.StatusOK, map[string]any{"presets": abrPresets, "gpu": hw.GPUName, "gpu_vendor": string(hw.Vendor), "count": len(abrPresets)})
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	active := int32(0)
	if abrCoordinatorV2 != nil {
		active = abrCoordinatorV2.Active()
	}
	rejections := uint64(0)
	if abrCoordinatorV2 != nil {
		rejections = abrCoordinatorV2.gpuAdmissionRejected.Load()
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "gpu": hw.GPUName, "vram_mb": hw.VRAM_MB, "active_jobs": active, "max_jobs": maxQueueSize, "presets": len(abrPresets), "gpu_admission_configured": gpuAdmission != nil && gpuAdmission.Configured(), "gpu_admission_rejections": rejections})
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

func handleRunnerContractV2(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"capability_id": "video:transcode.abr", "protocol": "paid-job/v1", "transports": []string{"stream"},
		"work_unit": map[string]any{"name": ABRWorkUnitV2, "extractor": map[string]any{"type": "response-trailer", "trailer": ABRWorkUnitsTrailerV2}},
		"paths":     map[string]string{"invoke": "/v1/video/transcode/abr"}, "readiness": map[string]string{"type": "http-status", "path": "/healthz"},
		"identity": map[string]string{"provider": "abr-runner"}, "schema_versions": map[string]string{"paid-job/v1": "1.0.15", ABRRequestSchemaV2: "2.0.0"},
	})
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Print(transcode.BuildSummary("abr-runner"))
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
	all, err := transcode.LoadABRPresetsFromBytes(presetBody)
	if err != nil {
		log.Fatalf("load presets: %v", err)
	}
	var skipped []string
	abrPresets, skipped = transcode.ValidateABRPresets(all, hw)
	if len(abrPresets) == 0 {
		log.Fatal("no ABR presets are supported by this runner")
	}
	log.Printf("abr-runner: %d active presets (%d skipped)", len(abrPresets), len(skipped))
	store, err := NewFileWorkloadStoreV2(stateDir)
	if err != nil {
		log.Fatalf("initialize workload state: %v", err)
	}
	executor, err := NewDurableABRExecutorV2(store, hw)
	if err != nil {
		log.Fatal(err)
	}
	gpuAdmission, err = transcode.NewGPUAdmissionGate(os.Getenv("GPU_ADMISSION_LOCK"))
	if err != nil {
		log.Fatalf("initialize GPU admission: %v", err)
	}
	abrCoordinatorV2, err = NewABRExecutionCoordinatorV2(store, executor, maxQueueSize, gpuAdmission)
	if err != nil {
		log.Fatal(err)
	}
	handler, err := NewABRHandlerV2(store, abrCoordinatorV2, abrPresets)
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/video/transcode/abr", handler)
	mux.HandleFunc("/.well-known/livepeer-runner", handleRunnerContractV2)
	mux.HandleFunc("/v1/video/transcode/abr/presets", handlePresets)
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
