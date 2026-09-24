package liverunner

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

var ErrMediaRouterExitedV1 = errors.New("media_router_exited")

type mediaMTXCommandFactoryV1 func(binary, configPath string) *exec.Cmd

type MediaMTXSupervisorV1 struct {
	binary     string
	configPath string
	config     []byte
	apiBase    string
	client     *http.Client
	poll       time.Duration
	command    mediaMTXCommandFactoryV1

	mu       sync.RWMutex
	process  *exec.Cmd
	done     chan struct{}
	exitErr  error
	ready    bool
	stopping bool
}

func NewMediaMTXSupervisorV1(binary, configPath string, config MediaMTXConfigV1, apiTimeout, pollInterval time.Duration) (*MediaMTXSupervisorV1, error) {
	if binary == "" || configPath == "" || apiTimeout <= 0 || pollInterval <= 0 {
		return nil, errors.New("MediaMTX supervisor configuration is incomplete")
	}
	rendered, err := RenderMediaMTXConfigV1(config)
	if err != nil {
		return nil, err
	}
	router, err := NewMediaRouterClientV1("http://"+config.APIAddress, nil, apiTimeout)
	if err != nil {
		return nil, err
	}
	return &MediaMTXSupervisorV1{
		binary: binary, configPath: configPath, config: rendered,
		apiBase: router.baseURL, client: router.client, poll: pollInterval,
		command: func(binary, configPath string) *exec.Cmd { return exec.Command(binary, configPath) },
	}, nil
}

func (s *MediaMTXSupervisorV1) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.process != nil {
		s.mu.Unlock()
		return errors.New("MediaMTX process already started")
	}
	if err := writePrivateFileAtomicV1(s.configPath, s.config); err != nil {
		s.mu.Unlock()
		return errors.New("write MediaMTX configuration failed")
	}
	command := s.command(s.binary, s.configPath)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		s.mu.Unlock()
		return errors.New("start MediaMTX process failed")
	}
	s.process = command
	s.done = make(chan struct{})
	s.exitErr = nil
	s.ready = false
	s.stopping = false
	done := s.done
	s.mu.Unlock()

	go s.wait(command, done)
	for {
		if s.probe(ctx) {
			s.mu.Lock()
			if s.process == command && !s.stopping {
				s.ready = true
				s.mu.Unlock()
				go s.monitor(command, done)
				return nil
			}
			s.mu.Unlock()
		}
		timer := time.NewTimer(s.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.Stop(stopContext)
			cancel()
			return ctx.Err()
		case <-done:
			timer.Stop()
			return ErrMediaRouterExitedV1
		case <-timer.C:
		}
	}
}

func (s *MediaMTXSupervisorV1) monitor(command *exec.Cmd, done <-chan struct{}) {
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			ready := s.probe(context.Background())
			s.mu.Lock()
			if s.process == command && !s.stopping {
				s.ready = ready
			}
			s.mu.Unlock()
		}
	}
}

func (s *MediaMTXSupervisorV1) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ready
}

func (s *MediaMTXSupervisorV1) Done() <-chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.done
}

func (s *MediaMTXSupervisorV1) Err() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.exitErr
}

func (s *MediaMTXSupervisorV1) Stop(ctx context.Context) error {
	s.mu.Lock()
	command := s.process
	done := s.done
	if command == nil {
		s.mu.Unlock()
		return nil
	}
	s.stopping = true
	s.ready = false
	process := command.Process
	s.mu.Unlock()

	if process != nil {
		_ = process.Signal(syscall.SIGTERM)
	}
	select {
	case <-done:
		return s.Err()
	case <-ctx.Done():
		if process != nil {
			_ = process.Kill()
		}
		<-done
		return ctx.Err()
	}
}

func (s *MediaMTXSupervisorV1) wait(command *exec.Cmd, done chan struct{}) {
	err := command.Wait()
	s.mu.Lock()
	if s.process == command {
		if s.stopping {
			err = nil
		} else {
			err = ErrMediaRouterExitedV1
		}
		s.exitErr = err
		s.ready = false
	}
	close(done)
	s.mu.Unlock()
}

func (s *MediaMTXSupervisorV1) probe(ctx context.Context) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiBase+"/v3/paths/list", nil)
	if err != nil {
		return false
	}
	response, err := s.client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return response.StatusCode >= 200 && response.StatusCode < 300
}

func writePrivateFileAtomicV1(target string, body []byte) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".mediamtx-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, target)
}
