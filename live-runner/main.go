package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

func main() {
	cfg := loadConfig()
	log.Print(transcode.BuildSummary("live-runner"))
	log.Printf("live-runner config %s", cfg.logFields())
	srv, err := newServer(cfg)
	if err != nil {
		log.Fatalf("init live-runner: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.run(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("live-runner: %v", err)
	}
}
