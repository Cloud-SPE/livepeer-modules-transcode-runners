package liverunner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

const maxRunnerRequestBodyV1 = 256 * 1024

type LiveSessionRuntimeV1 interface {
	ValidateSession(RunnerCreateRequestV1) error
	EnsureSession(context.Context, SessionRecordV1, SessionSecretsV1) error
	TerminateSession(context.Context, SessionRecordV1) error
	ActivateStreamKey(context.Context, SessionRecordV1) error
}

type RunnerResponseFactoryV1 struct {
	PublicRTMPBase string
	PublicHTTPBase string
	GrantTTL       time.Duration
	Now            func() time.Time
	Random         func(int) ([]byte, error)
}

func (f RunnerResponseFactoryV1) Create(_ RunnerCreateRequestV1) (RunnerCreateResponseV1, error) {
	if err := f.Validate(); err != nil {
		return RunnerCreateResponseV1{}, err
	}
	runnerID, err := f.randomHexV1("runner_", 16)
	if err != nil {
		return RunnerCreateResponseV1{}, errors.New("runner session identity generation failed")
	}
	grantID, err := f.randomHexV1("grant_", 16)
	if err != nil {
		return RunnerCreateResponseV1{}, errors.New("runner grant identity generation failed")
	}
	grantSecret, err := f.randomHexV1("", 32)
	if err != nil {
		return RunnerCreateResponseV1{}, errors.New("runner grant generation failed")
	}
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	response := RunnerCreateResponseV1{
		RunnerSessionID: runnerID,
		Runtime: RuntimeDescriptorV1{
			Schema: RuntimeSchemaV1,
			Public: RuntimePublicV1{
				RTMPURL:     joinPublicURLV1(f.PublicRTMPBase, "ingest"),
				HLSURL:      joinPublicURLV1(f.PublicHTTPBase, "v1/public/sessions/"+url.PathEscape(runnerID)+"/master.m3u8"),
				KeyIssueURL: joinPublicURLV1(f.PublicHTTPBase, "v1/sessions/"+url.PathEscape(runnerID)+"/stream-keys"),
				StatusURL:   joinPublicURLV1(f.PublicHTTPBase, "v1/public/sessions/"+url.PathEscape(runnerID)+"/status"),
			},
			Grants: []GrantV1{{ID: grantID, Operations: []string{GrantOperationV1}, Secret: grantSecret, ExpiresAt: now().Add(f.GrantTTL).UTC().Format(time.RFC3339)}},
		},
	}
	if err := ValidateCreateResponseV1(response); err != nil {
		return RunnerCreateResponseV1{}, err
	}
	return response, nil
}

func (f RunnerResponseFactoryV1) Validate() error {
	if err := validateURLScheme(f.PublicRTMPBase, "rtmp", "rtmps"); err != nil {
		return errors.New("public RTMP URL is invalid")
	}
	rtmpURL, _ := url.Parse(f.PublicRTMPBase)
	if strings.TrimRight(rtmpURL.Path, "/") != "" || rtmpURL.RawQuery != "" || rtmpURL.Fragment != "" {
		return errors.New("public RTMP URL must be an origin without a path")
	}
	if err := validatePublicHTTPBaseV1(f.PublicHTTPBase); err != nil || f.GrantTTL <= 0 {
		return errors.New("public HTTP base URL or grant TTL is invalid")
	}
	return nil
}

func (f RunnerResponseFactoryV1) randomHexV1(prefix string, size int) (string, error) {
	if f.Random != nil {
		body, err := f.Random(size)
		if err != nil || len(body) != size {
			return "", errors.New("random source failed")
		}
		return prefix + hex.EncodeToString(body), nil
	}
	body := make([]byte, size)
	if _, err := rand.Read(body); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(body), nil
}

type LiveRunnerServerV1 struct {
	Store       *EncryptedFileSessionStoreV1
	Runtime     LiveSessionRuntimeV1
	Factory     RunnerResponseFactoryV1
	BrokerToken string
	KeyTTL      time.Duration
	Now         func() time.Time
	Random      func(int) ([]byte, error)
	Ready       func() bool
	HLS         http.Handler
	locksMu     sync.Mutex
	locks       map[string]*sync.Mutex
}

func (s *LiveRunnerServerV1) Handler(mediaAuthorizer http.Handler) (http.Handler, error) {
	if s.Store == nil || s.Runtime == nil || (s.BrokerToken != "" && len(s.BrokerToken) < 32) || s.KeyTTL <= 0 || mediaAuthorizer == nil {
		return nil, errors.New("live runner server dependencies are incomplete")
	}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/session-creates/reconcile", s.brokerAuthV1(http.HandlerFunc(s.handleReconcileCreateV1)))
	mux.Handle("POST /v1/sessions", s.brokerAuthV1(http.HandlerFunc(s.handleCreateV1)))
	mux.Handle("GET /v1/sessions/{id}", s.brokerAuthV1(http.HandlerFunc(s.handleStatusV1)))
	mux.HandleFunc("GET /v1/public/sessions/{id}/status", s.handleStatusV1)
	mux.HandleFunc("HEAD /v1/public/sessions/{id}/status", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Allow", "GET")
		writeRunnerErrorV1(writer, http.StatusMethodNotAllowed, "method_not_allowed")
	})
	if s.HLS != nil {
		mux.Handle("GET /v1/public/sessions/{id}/master.m3u8", s.HLS)
		mux.Handle("OPTIONS /v1/public/sessions/{id}/master.m3u8", s.HLS)
		mux.Handle("OPTIONS /v1/public/sessions/{id}/{rendition}/{asset}", s.HLS)
		mux.Handle("GET /v1/public/sessions/{id}/{rendition}/{asset}", s.HLS)
	}
	mux.Handle("DELETE /v1/sessions/{id}", s.brokerAuthV1(http.HandlerFunc(s.handleTerminateV1)))
	mux.HandleFunc("POST /v1/sessions/{id}/stream-keys", s.handleStreamKeyV1)
	mux.HandleFunc("GET /.well-known/livepeer-runner", s.handleRunnerContractV1)
	mux.HandleFunc("GET /ready", func(writer http.ResponseWriter, _ *http.Request) {
		if s.Ready != nil && !s.Ready() {
			writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "media_router_unavailable")
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("POST /internal/mediamtx/auth", mediaAuthorizer)
	return mux, nil
}

func (s *LiveRunnerServerV1) handleCreateV1(writer http.ResponseWriter, request *http.Request) {
	var create RunnerCreateRequestV1
	if err := decodeRunnerRequestV1(writer, request, &create); err != nil || ValidateCreateRequestV1(create) != nil {
		writeRunnerErrorV1(writer, http.StatusBadRequest, "invalid_request")
		return
	}
	unlockCreate := s.lockSessionV1("create:" + create.SessionID)
	defer unlockCreate()
	if err := s.Runtime.ValidateSession(create); err != nil {
		writeRunnerErrorV1(writer, http.StatusBadRequest, "invalid_session_params")
		return
	}
	response, err := s.Factory.Create(create)
	if err != nil {
		writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "runtime_unavailable")
		return
	}
	record, secrets, _, err := s.Store.CreateOrReplay(create, response)
	if errors.Is(err, ErrSessionIDReuseV1) {
		writeRunnerErrorV1(writer, http.StatusConflict, ErrSessionIDReuseV1.Error())
		return
	}
	if errors.Is(err, ErrSessionTerminalV1) || errors.Is(err, ErrCreateFencedV1) {
		writeRunnerErrorV1(writer, http.StatusGone, err.Error())
		return
	}
	if err != nil {
		writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "state_unavailable")
		return
	}
	unlock := s.lockSessionV1(record.RunnerSessionID)
	defer unlock()
	record, currentSecrets, err := s.Store.Load(record.BrokerSessionID)
	if err != nil {
		writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "state_unavailable")
		return
	}
	if record.State != "active" || record.Stopping || currentSecrets == nil {
		writeRunnerErrorV1(writer, http.StatusGone, ErrSessionTerminalV1.Error())
		return
	}
	secrets = *currentSecrets
	if err := s.Runtime.EnsureSession(request.Context(), record, secrets); err != nil {
		if errors.Is(err, transcode.ErrGPUAdmissionCapacity) {
			// A definitive refusal must also prevent an exact delayed replay
			// from starting after the broker releases its capacity slot.
			stopping, _, stopErr := s.Store.BeginTermination(record.BrokerSessionID, "capacity_exhausted")
			if stopErr == nil {
				_, stopErr = CompleteLiveTerminationV1(context.WithoutCancel(request.Context()), s.Store, s.Runtime, stopping, s.nowV1())
			}
			if stopErr != nil {
				writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "runtime_unavailable")
				return
			}
			writeRunnerErrorV1(writer, http.StatusTooManyRequests, "capacity_reached")
			return
		}
		writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "runtime_unavailable")
		return
	}
	writeRunnerJSONV1(writer, http.StatusOK, secrets.CreateResponse)
}

func (s *LiveRunnerServerV1) handleStatusV1(writer http.ResponseWriter, request *http.Request) {
	record, _, err := s.Store.LoadByRunnerSessionID(request.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		writeRunnerErrorV1(writer, http.StatusNotFound, "session_not_found")
		return
	}
	if err != nil {
		writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "state_unavailable")
		return
	}
	status := RunnerStatusV1{RunnerSessionID: record.RunnerSessionID, State: record.State, Usage: UsageV1{Unit: WorkUnitV1, Total: record.UsageTotal}, LastSequence: record.LastSequence, CloseReason: record.CloseReason, OutputState: record.OutputState, LastFailureCode: record.LastLadderFailureCode, CallbackRejectedTotal: record.CallbackRejectedTotal}
	if count := len(record.CallbackDeadLetters); count > 0 {
		letter := record.CallbackDeadLetters[count-1]
		status.LastCallbackRejection = &CallbackRejectionStatusV1{EventType: letter.Event.EventType, StatusCode: letter.StatusCode, ErrorCode: letter.ErrorCode, RejectedAt: letter.RejectedAt}
	}
	writeRunnerJSONV1(writer, http.StatusOK, status)
}

func (s *LiveRunnerServerV1) handleTerminateV1(writer http.ResponseWriter, request *http.Request) {
	var terminate TerminateRequestV1
	if err := decodeRunnerRequestV1(writer, request, &terminate); err != nil || !validCloseReasonV1(terminate.Reason) {
		writeRunnerErrorV1(writer, http.StatusBadRequest, "invalid_request")
		return
	}
	unlock := s.lockSessionV1(request.PathValue("id"))
	defer unlock()
	record, _, err := s.Store.LoadByRunnerSessionID(request.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "state_unavailable")
		return
	}
	if record.State == "active" {
		record, _, err = s.Store.BeginTermination(record.BrokerSessionID, terminate.Reason)
		if err != nil {
			writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "state_unavailable")
			return
		}
		record, err = CompleteLiveTerminationV1(request.Context(), s.Store, s.Runtime, record, s.nowV1())
		if err != nil {
			writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "runtime_unavailable")
			return
		}
	}
	writeRunnerJSONV1(writer, http.StatusOK, TerminateResponseV1{RunnerSessionID: record.RunnerSessionID, State: record.State, CloseReason: record.CloseReason})
}

func (s *LiveRunnerServerV1) handleStreamKeyV1(writer http.ResponseWriter, request *http.Request) {
	unlock := s.lockSessionV1(request.PathValue("id"))
	defer unlock()
	record, secrets, err := s.Store.LoadByRunnerSessionID(request.PathValue("id"))
	if err != nil || secrets == nil || record.State != "active" || record.Stopping {
		writeRunnerErrorV1(writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	token, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	if !ok || !secureEqualV1(secretSHA256V1(token), record.GrantAudit.SecretSHA256) {
		writeRunnerErrorV1(writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	grantExpiry, err := time.Parse(time.RFC3339, record.GrantAudit.ExpiresAt)
	if err != nil || !s.nowV1().Before(grantExpiry) {
		writeRunnerErrorV1(writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	var issue StreamKeyIssueRequestV1
	if err := decodeRunnerRequestV1(writer, request, &issue); err != nil || ValidateStreamKeyIssueRequestV1(issue) != nil || issue.Audience != secrets.CreateRequest.SessionParams.PublisherMode {
		writeRunnerErrorV1(writer, http.StatusBadRequest, "invalid_request")
		return
	}
	secret, err := s.randomHexV1(32)
	if err != nil {
		writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "credential_generation_failed")
		return
	}
	streamKey, err := BuildPrivateIngestStreamKeyV1(record.RunnerSessionID, secret)
	if err != nil {
		writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "credential_generation_failed")
		return
	}
	expiresAt := s.nowV1().Add(s.KeyTTL)
	if expiresAt.After(grantExpiry) {
		expiresAt = grantExpiry
	}
	response := StreamKeyIssueResponseV1{RequestID: issue.RequestID, StreamKey: streamKey, ExpiresAt: expiresAt.UTC().Format(time.RFC3339)}
	response, _, err = s.Store.RecordKeyIssue(record.BrokerSessionID, issue, response)
	switch {
	case errors.Is(err, ErrRequestIDReuseV1):
		writeRunnerErrorV1(writer, http.StatusConflict, ErrRequestIDReuseV1.Error())
	case errors.Is(err, ErrRequestSupersededV1):
		writeRunnerErrorV1(writer, http.StatusConflict, ErrRequestSupersededV1.Error())
	case errors.Is(err, ErrKeyActivationInFlightV1):
		writeRunnerErrorV1(writer, http.StatusConflict, ErrKeyActivationInFlightV1.Error())
	case err != nil:
		writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "state_unavailable")
	default:
		current, _, loadErr := s.Store.Load(record.BrokerSessionID)
		if loadErr != nil {
			writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "state_unavailable")
			return
		}
		if current.PendingKeyActivationID == issue.RequestID {
			if err := s.Runtime.ActivateStreamKey(request.Context(), current); err != nil {
				writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "runtime_unavailable")
				return
			}
			if err := s.Store.CompleteKeyActivation(record.BrokerSessionID, issue.RequestID); err != nil {
				writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "state_unavailable")
				return
			}
		}
		writeRunnerJSONV1(writer, http.StatusOK, response)
	}
}

func (s *LiveRunnerServerV1) handleRunnerContractV1(writer http.ResponseWriter, _ *http.Request) {
	writeRunnerJSONV1(writer, http.StatusOK, LiveRunnerContractV1())
}

func (s *LiveRunnerServerV1) brokerAuthV1(next http.Handler) http.Handler {
	if s.BrokerToken == "" {
		// No token configured: the session routes are reached only over the
		// agent's tunnel, which carries no bearer. See LoadLiveRunnerConfigV1.
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		token, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
		if !ok || !secureEqualV1(token, s.BrokerToken) {
			writeRunnerErrorV1(writer, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (s *LiveRunnerServerV1) nowV1() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *LiveRunnerServerV1) randomHexV1(size int) (string, error) {
	factory := RunnerResponseFactoryV1{Random: s.Random}
	return factory.randomHexV1("", size)
}

func (s *LiveRunnerServerV1) lockSessionV1(id string) func() {
	s.locksMu.Lock()
	if s.locks == nil {
		s.locks = make(map[string]*sync.Mutex)
	}
	lock := s.locks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		s.locks[id] = lock
	}
	s.locksMu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func decodeRunnerRequestV1(writer http.ResponseWriter, request *http.Request, value any) error {
	defer request.Body.Close()
	request.Body = http.MaxBytesReader(writer, request.Body, maxRunnerRequestBodyV1)
	return DecodeStrictV1(request.Body, value)
}

func writeRunnerJSONV1(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeRunnerErrorV1(writer http.ResponseWriter, status int, code string) {
	writeRunnerJSONV1(writer, status, map[string]string{"error": code})
}

func joinPublicURLV1(base, suffix string) string { return strings.TrimRight(base, "/") + "/" + suffix }

func validatePublicHTTPBaseV1(raw string) error {
	if err := validateHTTPURL(raw); err != nil {
		return err
	}
	parsed, _ := url.Parse(raw)
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("public HTTP base cannot contain a query or fragment")
	}
	return nil
}
