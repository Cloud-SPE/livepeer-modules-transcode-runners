package liverunner

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestMediaMTXSupervisorStartsReadyAndStops(t *testing.T) {
	api := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v3/paths/list" {
			t.Errorf("readiness path=%q", request.URL.Path)
		}
		writer.WriteHeader(http.StatusOK)
	}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	api.Listener = listener
	api.Start()
	defer api.Close()
	config := DefaultMediaMTXConfigV1("http://127.0.0.1:8080/internal/mediamtx/auth")
	config.APIAddress = listener.Addr().String()
	configPath := filepath.Join(t.TempDir(), "private", "mediamtx.yml")
	supervisor, err := NewMediaMTXSupervisorV1("mediamtx", configPath, config, time.Second, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	supervisor.command = mediaMTXHelperCommandV1
	startContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Start(startContext); err != nil {
		t.Fatal(err)
	}
	if !supervisor.Ready() {
		t.Fatal("supervisor did not become ready")
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions=%v", info.Mode().Perm())
	}
	api.Close()
	deadline := time.Now().Add(time.Second)
	for supervisor.Ready() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if supervisor.Ready() {
		t.Fatal("supervisor remained ready after the MediaMTX API stopped")
	}
	stopContext, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := supervisor.Stop(stopContext); err != nil {
		t.Fatal(err)
	}
	if supervisor.Ready() || supervisor.Err() != nil {
		t.Fatalf("stopped supervisor ready=%v err=%v", supervisor.Ready(), supervisor.Err())
	}
}

func TestMediaMTXSupervisorReportsUnexpectedExit(t *testing.T) {
	config := DefaultMediaMTXConfigV1("http://127.0.0.1:8080/internal/mediamtx/auth")
	config.APIAddress = "127.0.0.1:19997"
	supervisor, err := NewMediaMTXSupervisorV1("mediamtx", filepath.Join(t.TempDir(), "mediamtx.yml"), config, 10*time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	supervisor.command = func(_, _ string) *exec.Cmd { return exec.Command("sh", "-c", "exit 7") }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Start(ctx); !errors.Is(err, ErrMediaRouterExitedV1) {
		t.Fatalf("unexpected exit error=%v", err)
	}
	if supervisor.Ready() || !errors.Is(supervisor.Err(), ErrMediaRouterExitedV1) {
		t.Fatalf("unexpected exit ready=%v err=%v", supervisor.Ready(), supervisor.Err())
	}
}

func TestMediaMTXSupervisorHelperProcess(t *testing.T) {
	if os.Getenv("LIVE_RUNNER_MEDIAMTX_HELPER") != "1" {
		return
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	<-signals
	os.Exit(0)
}

func mediaMTXHelperCommandV1(_, _ string) *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=^TestMediaMTXSupervisorHelperProcess$")
	command.Env = append(os.Environ(), "LIVE_RUNNER_MEDIAMTX_HELPER=1")
	return command
}
