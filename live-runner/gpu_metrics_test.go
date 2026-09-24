package liverunner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNVIDIASMIPressureSamplerUsesBoundedSafeQuery(t *testing.T) {
	var gotName string
	var gotArgs []string
	sampler := NVIDIASMIPressureSamplerV1{
		Binary: "nvidia-smi-test", Timeout: time.Second,
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			gotName, gotArgs = name, append([]string(nil), args...)
			return []byte("2, 1024\n3, 2048\n"), nil
		},
	}
	pressure, err := sampler.Sample(context.Background())
	if err != nil || pressure.GPUCount != 2 || pressure.EncoderSessions != 5 || pressure.MemoryUsedMiB != 3072 {
		t.Fatalf("pressure=%+v err=%v", pressure, err)
	}
	joined := strings.Join(gotArgs, " ")
	if gotName != "nvidia-smi-test" || !strings.Contains(joined, "encoder.stats.sessionCount,memory.used") || !strings.Contains(joined, "noheader,nounits") {
		t.Fatalf("command=%s %v", gotName, gotArgs)
	}
}

func TestNVIDIASMIPressureSamplerTimesOutAndRejectsMalformedOutput(t *testing.T) {
	sampler := NVIDIASMIPressureSamplerV1{
		Timeout: time.Millisecond,
		run: func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	if _, err := sampler.Sample(context.Background()); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("timeout error=%v", err)
	}
	for _, body := range []string{"", "N/A, 12", "1", "1, nope"} {
		if _, err := parseNVIDIASMIPressureV1(body); err == nil {
			t.Fatalf("malformed pressure accepted: %q", body)
		}
	}
	failing := NVIDIASMIPressureSamplerV1{Timeout: time.Second, run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("unsafe driver detail")
	}}
	if _, err := failing.Sample(context.Background()); err == nil || strings.Contains(err.Error(), "driver") {
		t.Fatalf("unsafe sampler error=%v", err)
	}
}
