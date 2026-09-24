package transcode

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestGPUAdmissionAllowsSameCohortAndRejectsCrossCohort(t *testing.T) {
	gate := testGPUAdmissionGate(t, filepath.Join(t.TempDir(), "gpu.lock"))
	batchOne, err := gate.Acquire(GPUAdmissionBatch)
	if err != nil {
		t.Fatal(err)
	}
	defer batchOne.Close()
	batchTwo, err := gate.Acquire(GPUAdmissionBatch)
	if err != nil {
		t.Fatalf("second batch lease: %v", err)
	}
	if _, err := gate.Acquire(GPUAdmissionLive); !errors.Is(err, ErrGPUAdmissionCapacity) {
		t.Fatalf("live with batch leases: %v, want capacity", err)
	}
	if err := batchTwo.Close(); err != nil {
		t.Fatal(err)
	}
	if err := batchOne.Close(); err != nil {
		t.Fatal(err)
	}
	liveOne, err := gate.Acquire(GPUAdmissionLive)
	if err != nil {
		t.Fatalf("live after batch release: %v", err)
	}
	defer liveOne.Close()
	liveTwo, err := gate.Acquire(GPUAdmissionLive)
	if err != nil {
		t.Fatalf("second live lease: %v", err)
	}
	defer liveTwo.Close()
	if _, err := gate.Acquire(GPUAdmissionBatch); !errors.Is(err, ErrGPUAdmissionCapacity) {
		t.Fatalf("batch with live leases: %v, want capacity", err)
	}
}

func TestGPUAdmissionKeysAreIsolated(t *testing.T) {
	directory := t.TempDir()
	first := testGPUAdmissionGate(t, filepath.Join(directory, "gpu-one.lock"))
	second := testGPUAdmissionGate(t, filepath.Join(directory, "gpu-two.lock"))
	leaseOne, err := first.Acquire(GPUAdmissionLive)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseOne.Close()
	leaseTwo, err := second.Acquire(GPUAdmissionLive)
	if err != nil {
		t.Fatalf("different GPU key was blocked: %v", err)
	}
	defer leaseTwo.Close()
}

func TestGPUAdmissionOpposingCohortsCannotRaceIntoOneDomain(t *testing.T) {
	for attempt := 0; attempt < 100; attempt++ {
		gate := testGPUAdmissionGate(t, filepath.Join(t.TempDir(), "gpu.lock"))
		start := make(chan struct{})
		release := make(chan struct{})
		results := make(chan error, 2)
		var workers sync.WaitGroup
		for _, cohort := range []GPUAdmissionCohort{GPUAdmissionBatch, GPUAdmissionLive} {
			workers.Add(1)
			go func(cohort GPUAdmissionCohort) {
				defer workers.Done()
				<-start
				lease, err := gate.Acquire(cohort)
				results <- err
				if err == nil {
					<-release
					_ = lease.Close()
				}
			}(cohort)
		}
		close(start)
		first, second := <-results, <-results
		close(release)
		workers.Wait()
		if (first == nil) == (second == nil) {
			t.Fatalf("attempt %d results = %v, %v; want one admitted cohort", attempt, first, second)
		}
		if first != nil && !errors.Is(first, ErrGPUAdmissionCapacity) {
			t.Fatalf("attempt %d first error = %v", attempt, first)
		}
		if second != nil && !errors.Is(second, ErrGPUAdmissionCapacity) {
			t.Fatalf("attempt %d second error = %v", attempt, second)
		}
	}
}

func TestGPUAdmissionProcessExitReleasesLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpu.lock")
	gate := testGPUAdmissionGate(t, path)
	command := exec.Command(os.Args[0], "-test.run=TestGPUAdmissionLeaseHelper")
	command.Env = append(os.Environ(), "GPU_ADMISSION_HELPER_PATH="+path)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "acquired live\n" {
		t.Fatalf("helper readiness = %q, %v", line, err)
	}
	if _, err := gate.Acquire(GPUAdmissionBatch); !errors.Is(err, ErrGPUAdmissionCapacity) {
		t.Fatalf("lease while helper runs: %v, want capacity", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	lease, err := gate.Acquire(GPUAdmissionLive)
	if err != nil {
		t.Fatalf("lease after helper exit: %v", err)
	}
	defer lease.Close()
}

func TestGPUAdmissionLeaseHelper(t *testing.T) {
	path := os.Getenv("GPU_ADMISSION_HELPER_PATH")
	if path == "" {
		return
	}
	gate := testGPUAdmissionGate(t, path)
	cohort := GPUAdmissionLive
	if os.Getenv("GPU_ADMISSION_HELPER_COHORT") == "batch" {
		cohort = GPUAdmissionBatch
	}
	lease, err := gate.Acquire(cohort)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	name := "live"
	if cohort == GPUAdmissionBatch {
		name = "batch"
	}
	fmt.Println("acquired", name)
	if raw := os.Getenv("GPU_ADMISSION_HELPER_HOLD"); raw != "" {
		duration, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(duration)
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestGPUAdmissionConfigurationValidation(t *testing.T) {
	disabled, err := NewGPUAdmissionGate("")
	if err != nil || disabled.Configured() {
		t.Fatalf("disabled gate = %#v, %v", disabled, err)
	}
	if lease, err := disabled.Acquire(GPUAdmissionLive); err != nil || lease == nil {
		t.Fatalf("disabled acquire = %#v, %v", lease, err)
	}
	if _, err := NewGPUAdmissionGate("relative.lock"); err == nil {
		t.Fatal("relative path accepted")
	}
	missing := filepath.Join(t.TempDir(), "missing", "gpu.lock")
	if _, err := NewGPUAdmissionGate(missing); err == nil {
		t.Fatal("missing directory accepted")
	}
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(directory, "gpu.lock")
	symlink := base + ".live"
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := NewGPUAdmissionGate(base); err == nil {
		t.Fatal("symlink lock accepted")
	}
}

func testGPUAdmissionGate(t *testing.T, path string) *GPUAdmissionGate {
	t.Helper()
	gate, err := NewGPUAdmissionGate(path)
	if err != nil {
		t.Fatal(err)
	}
	return gate
}
