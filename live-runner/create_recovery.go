package liverunner

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"net/http"
	"os"
	"path/filepath"
)

var ErrCreateFencedV1 = errors.New("session_create_fenced")

type CreateResolutionV1 struct {
	SessionID       string `json:"session_id"`
	Outcome         string `json:"outcome"`
	RunnerSessionID string `json:"runner_session_id,omitempty"`
}

func (s *EncryptedFileSessionStoreV1) createFenceMAC(id string) []byte {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("session-create-fence/v1:" + id))
	return mac.Sum(nil)
}
func (s *EncryptedFileSessionStoreV1) createFencedLocked(id string) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(s.dir, id+".create-fence"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !hmac.Equal(raw, s.createFenceMAC(id)) {
		return false, errors.New("create fence integrity check failed")
	}
	return true, nil
}

// ReconcileCreate serializes with CreateOrReplay under the store lock. Its
// absent result is durable and prevents delayed create after broker recovery.
func (s *EncryptedFileSessionStoreV1) ReconcileCreate(id string) (CreateResolutionV1, error) {
	if !opaqueIDPattern.MatchString(id) {
		return CreateResolutionV1{}, errors.New("invalid broker session ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, _, err := s.loadLocked(id)
	if err == nil {
		return CreateResolutionV1{SessionID: id, Outcome: "created", RunnerSessionID: record.RunnerSessionID}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return CreateResolutionV1{}, err
	}
	fenced, err := s.createFencedLocked(id)
	if err != nil {
		return CreateResolutionV1{}, err
	}
	if !fenced {
		f, err := os.CreateTemp(s.dir, ".create-fence-*.tmp")
		if err != nil {
			return CreateResolutionV1{}, err
		}
		name := f.Name()
		defer os.Remove(name)
		if _, err = f.Write(s.createFenceMAC(id)); err != nil {
			f.Close()
			return CreateResolutionV1{}, err
		}
		if err = f.Sync(); err != nil {
			f.Close()
			return CreateResolutionV1{}, err
		}
		if err = f.Close(); err != nil {
			return CreateResolutionV1{}, err
		}
		if err = os.Rename(name, filepath.Join(s.dir, id+".create-fence")); err != nil {
			return CreateResolutionV1{}, err
		}
	}
	// Also sync on a retry after an earlier response/dir-sync failure.
	dir, err := os.Open(s.dir)
	if err != nil {
		return CreateResolutionV1{}, err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return CreateResolutionV1{}, err
	}
	return CreateResolutionV1{SessionID: id, Outcome: "fenced"}, nil
}

func (s *LiveRunnerServerV1) handleReconcileCreateV1(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := decodeRunnerRequestV1(w, r, &req); err != nil || !opaqueIDPattern.MatchString(req.SessionID) {
		writeRunnerErrorV1(w, http.StatusBadRequest, "invalid_request")
		return
	}
	// Wait for an executing create to leave its runtime critical section.
	unlock := s.lockSessionV1("create:" + req.SessionID)
	defer unlock()
	result, err := s.Store.ReconcileCreate(req.SessionID)
	if err != nil {
		writeRunnerErrorV1(w, http.StatusServiceUnavailable, "state_unavailable")
		return
	}
	writeRunnerJSONV1(w, http.StatusOK, result)
}
