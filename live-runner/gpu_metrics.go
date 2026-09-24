package liverunner

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const maxObservedGPUsV1 = 16

type GPUPressureV1 struct {
	GPUCount        uint64
	EncoderSessions uint64
	MemoryUsedMiB   uint64
}

type GPUPressureSamplerV1 interface {
	Sample(context.Context) (GPUPressureV1, error)
}

type NVIDIASMIPressureSamplerV1 struct {
	Binary  string
	Timeout time.Duration
	run     func(context.Context, string, ...string) ([]byte, error)
}

func (s NVIDIASMIPressureSamplerV1) Sample(ctx context.Context) (GPUPressureV1, error) {
	if s.Timeout <= 0 {
		return GPUPressureV1{}, errors.New("GPU telemetry timeout is invalid")
	}
	binary := s.Binary
	if binary == "" {
		binary = "nvidia-smi"
	}
	run := s.run
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		}
	}
	probeContext, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	body, err := run(probeContext, binary, "--query-gpu=encoder.stats.sessionCount,memory.used", "--format=csv,noheader,nounits")
	if err != nil {
		return GPUPressureV1{}, errors.New("GPU telemetry unavailable")
	}
	return parseNVIDIASMIPressureV1(string(body))
}

func parseNVIDIASMIPressureV1(body string) (GPUPressureV1, error) {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) == 0 || len(lines) > maxObservedGPUsV1 || len(lines) == 1 && strings.TrimSpace(lines[0]) == "" {
		return GPUPressureV1{}, errors.New("GPU telemetry output is invalid")
	}
	var pressure GPUPressureV1
	for _, line := range lines {
		parts := strings.Split(line, ",")
		if len(parts) != 2 {
			return GPUPressureV1{}, errors.New("GPU telemetry output is invalid")
		}
		sessions, err := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 64)
		if err != nil {
			return GPUPressureV1{}, errors.New("GPU telemetry encoder count is invalid")
		}
		memory, err := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			return GPUPressureV1{}, errors.New("GPU telemetry memory is invalid")
		}
		if ^uint64(0)-pressure.EncoderSessions < sessions || ^uint64(0)-pressure.MemoryUsedMiB < memory {
			return GPUPressureV1{}, errors.New("GPU telemetry total overflow")
		}
		pressure.GPUCount++
		pressure.EncoderSessions += sessions
		pressure.MemoryUsedMiB += memory
	}
	return pressure, nil
}
