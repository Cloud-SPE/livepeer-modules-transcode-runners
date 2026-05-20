package main

import (
	"errors"
	"sync"
	"time"
)

type sessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*sessionRuntime
	streams  map[string]string
}

func newSessionStore() *sessionStore {
	return &sessionStore{
		sessions: make(map[string]*sessionRuntime),
		streams:  make(map[string]string),
	}
}

func (s *sessionStore) add(rt *sessionRuntime) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[rt.rec.RunnerSessionID]; exists {
		return errors.New("session already exists")
	}
	s.sessions[rt.rec.RunnerSessionID] = rt
	if rt.rec.StreamKey != "" {
		s.streams[rt.rec.StreamKey] = rt.rec.RunnerSessionID
	}
	return nil
}

func (s *sessionStore) get(id string) *sessionRuntime {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[id]
}

func (s *sessionStore) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rt, ok := s.sessions[id]; ok {
		delete(s.streams, rt.rec.StreamKey)
		delete(s.sessions, id)
	}
}

func (s *sessionStore) byStreamKey(streamKey string) *sessionRuntime {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.streams[streamKey]
	if !ok {
		return nil
	}
	return s.sessions[id]
}

func (s *sessionStore) snapshot() []*sessionRuntime {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*sessionRuntime, 0, len(s.sessions))
	for _, rt := range s.sessions {
		out = append(out, rt)
	}
	return out
}

func (s *sessionStore) reap() {
	now := time.Now()
	for _, rt := range s.snapshot() {
		rt.mu.Lock()
		done := rt.rec.State == stateEnded || rt.rec.State == stateFailed
		endedAt := rt.rec.EndedAt
		rt.mu.Unlock()
		if done && !endedAt.IsZero() && now.Sub(endedAt) > 5*time.Minute {
			s.delete(rt.rec.RunnerSessionID)
		}
	}
}
