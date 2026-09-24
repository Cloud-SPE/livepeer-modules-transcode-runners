package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	liverunner "github.com/Cloud-SPE/livepeer-modules-transcode-runners/live-runner"
	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

func main() {
	log.Print(transcode.BuildSummary("live-runner"))
	config, err := liverunner.LoadLiveRunnerConfigV1(nil)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := liverunner.RunLiveRunnerV1(ctx, config); err != nil {
		log.Fatal(err)
	}
}
