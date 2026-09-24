package transcode

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// ErrGPUAdmissionCapacity means another process holds an incompatible lease
// for the configured physical GPU. Callers should report this as capacity,
// never as an encoder failure.
var ErrGPUAdmissionCapacity = errors.New("GPU admission capacity reached")

// GPUAdmissionCohort identifies compatible workloads. Members of one cohort
// may coexist up to their runner-local limits, but live and batch cohorts may
// not overlap on the same physical GPU.
type GPUAdmissionCohort uint8

const (
	GPUAdmissionBatch GPUAdmissionCohort = iota + 1
	GPUAdmissionLive
)

// GPUAdmissionGate coordinates runners through a lock file mounted from the
// same host volume. Linux flock leases are released by the kernel when the
// process exits, so a crashed runner cannot strand capacity.
type GPUAdmissionGate struct {
	basePath string
}

// NewGPUAdmissionGate validates a configured lock file. An empty path disables
// cross-process admission for deployments where runners do not share a GPU.
func NewGPUAdmissionGate(path string) (*GPUAdmissionGate, error) {
	if path == "" {
		return &GPUAdmissionGate{}, nil
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) == string(filepath.Separator) {
		return nil, errors.New("GPU admission lock must be a clean absolute file path")
	}
	parent := filepath.Dir(path)
	info, err := os.Stat(parent)
	if err != nil {
		return nil, fmt.Errorf("GPU admission directory is unavailable: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("GPU admission parent is not a directory")
	}
	for _, candidate := range gpuAdmissionPaths(path) {
		file, err := openGPUAdmissionFile(candidate)
		if err != nil {
			return nil, fmt.Errorf("GPU admission lock is unavailable: %w", err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("close GPU admission lock: %w", err)
		}
	}
	return &GPUAdmissionGate{basePath: path}, nil
}

func (g *GPUAdmissionGate) Configured() bool {
	return g != nil && g.basePath != ""
}

// Acquire serializes admission briefly, refuses an active opposing cohort,
// then holds a shared cohort lease for the complete workload lifetime.
func (g *GPUAdmissionGate) Acquire(cohort GPUAdmissionCohort) (*GPUAdmissionLease, error) {
	if g == nil || g.basePath == "" {
		return &GPUAdmissionLease{}, nil
	}
	var ownPath, opposingPath string
	switch cohort {
	case GPUAdmissionBatch:
		ownPath, opposingPath = g.basePath+".batch", g.basePath+".live"
	case GPUAdmissionLive:
		ownPath, opposingPath = g.basePath+".live", g.basePath+".batch"
	default:
		return nil, errors.New("invalid GPU admission cohort")
	}
	mutex, err := openGPUAdmissionFile(g.basePath + ".mutex")
	if err != nil {
		return nil, fmt.Errorf("open GPU admission mutex: %w", err)
	}
	if err := syscall.Flock(int(mutex.Fd()), syscall.LOCK_EX); err != nil {
		_ = mutex.Close()
		return nil, fmt.Errorf("acquire GPU admission mutex: %w", err)
	}
	defer func() {
		_ = syscall.Flock(int(mutex.Fd()), syscall.LOCK_UN)
		_ = mutex.Close()
	}()

	opposing, err := openGPUAdmissionFile(opposingPath)
	if err != nil {
		return nil, fmt.Errorf("open opposing GPU admission cohort: %w", err)
	}
	if err := syscall.Flock(int(opposing.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = opposing.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrGPUAdmissionCapacity
		}
		return nil, fmt.Errorf("probe opposing GPU admission cohort: %w", err)
	}
	_ = syscall.Flock(int(opposing.Fd()), syscall.LOCK_UN)
	_ = opposing.Close()

	own, err := openGPUAdmissionFile(ownPath)
	if err != nil {
		return nil, fmt.Errorf("open GPU admission cohort: %w", err)
	}
	if err := syscall.Flock(int(own.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		_ = own.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrGPUAdmissionCapacity
		}
		return nil, fmt.Errorf("acquire GPU admission cohort: %w", err)
	}
	return &GPUAdmissionLease{file: own}, nil
}

func gpuAdmissionPaths(basePath string) []string {
	return []string{basePath + ".mutex", basePath + ".batch", basePath + ".live"}
}

func openGPUAdmissionFile(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CLOEXEC|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_RDWR, 0o660)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Base(path)), nil
}

type GPUAdmissionLease struct {
	file *os.File
	once sync.Once
	err  error
}

func (l *GPUAdmissionLease) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if l.file == nil {
			return
		}
		if err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN); err != nil {
			l.err = fmt.Errorf("release GPU admission lock: %w", err)
		}
		if err := l.file.Close(); err != nil && l.err == nil {
			l.err = fmt.Errorf("close GPU admission lock: %w", err)
		}
	})
	return l.err
}
