package main

import "sync/atomic"

type runnerMetrics struct {
	outputCredentialPutSuccess atomic.Uint64
	outputCredentialPutFailure atomic.Uint64
	ingestAuthenticationReject atomic.Uint64
	ffmpegExitTotal            atomic.Uint64
}

type metricsSnapshot struct {
	OutputCredentialPutSuccess uint64 `json:"output_credential_put_success"`
	OutputCredentialPutFailure uint64 `json:"output_credential_put_failures"`
	IngestAuthenticationReject uint64 `json:"ingest_authentication_rejections"`
	FFmpegExitTotal            uint64 `json:"ffmpeg_exit_total"`
}

func newRunnerMetrics() *runnerMetrics {
	return &runnerMetrics{}
}

func (m *runnerMetrics) snapshot() metricsSnapshot {
	if m == nil {
		return metricsSnapshot{}
	}
	return metricsSnapshot{
		OutputCredentialPutSuccess: m.outputCredentialPutSuccess.Load(),
		OutputCredentialPutFailure: m.outputCredentialPutFailure.Load(),
		IngestAuthenticationReject: m.ingestAuthenticationReject.Load(),
		FFmpegExitTotal:            m.ffmpegExitTotal.Load(),
	}
}
