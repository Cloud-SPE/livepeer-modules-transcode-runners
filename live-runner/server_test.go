package liverunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

const testBrokerTokenV1 = "broker-token-0123456789abcdef0123"

type fakeLiveRuntimeV1 struct {
	mu              sync.Mutex
	ensureCount     int
	terminateCount  int
	failEnsure      bool
	ensureErr       error
	failTerminate   bool
	failValidation  bool
	activationCount int
	failActivation  bool
}

func (f *fakeLiveRuntimeV1) ValidateSession(RunnerCreateRequestV1) error {
	if f.failValidation {
		return errors.New("unsafe validation detail")
	}
	return nil
}

func (f *fakeLiveRuntimeV1) EnsureSession(context.Context, SessionRecordV1, SessionSecretsV1) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCount++
	if f.failEnsure {
		return errors.New("unsafe runtime detail")
	}
	if f.ensureErr != nil {
		return f.ensureErr
	}
	return nil
}

func TestLiveRunnerReportsGPUAdmissionAsCapacity(t *testing.T) {
	handler, _, runtime := testLiveRunnerHandlerV1(t)
	runtime.ensureErr = transcode.ErrGPUAdmissionCapacity
	create := readStrictFixtureV1[RunnerCreateRequestV1](t, "create-request.json")
	response := runnerRequestV1(t, handler, http.MethodPost, "/v1/sessions", create, testBrokerTokenV1)
	if response.Code != http.StatusTooManyRequests || !bytes.Contains(response.Body.Bytes(), []byte(`"error":"capacity_reached"`)) {
		t.Fatalf("capacity response=%d %s", response.Code, response.Body.String())
	}
}

func (f *fakeLiveRuntimeV1) TerminateSession(context.Context, SessionRecordV1) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminateCount++
	if f.failTerminate {
		return errors.New("unsafe runtime detail")
	}
	return nil
}

func (f *fakeLiveRuntimeV1) ActivateStreamKey(context.Context, SessionRecordV1) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activationCount++
	if f.failActivation {
		return errors.New("unsafe activation detail")
	}
	return nil
}

func TestLiveRunnerCreateReplayStatusAndTerminate(t *testing.T) {
	handler, store, runtime := testLiveRunnerHandlerV1(t)
	create := readStrictFixtureV1[RunnerCreateRequestV1](t, "create-request.json")
	create.WorkID = "loc-auth:ce3fa091-844e-4004-b532-39d7d935061f"

	unauthorized := runnerRequestV1(t, handler, http.MethodPost, "/v1/sessions", create, "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized create=%d", unauthorized.Code)
	}
	first := runnerRequestV1(t, handler, http.MethodPost, "/v1/sessions", create, testBrokerTokenV1)
	if first.Code != http.StatusOK {
		t.Fatalf("create=%d %s", first.Code, first.Body.String())
	}
	var created RunnerCreateResponseV1
	if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil || ValidateCreateResponseV1(created) != nil {
		t.Fatalf("create response=%+v err=%v", created, err)
	}
	replay := runnerRequestV1(t, handler, http.MethodPost, "/v1/sessions", create, testBrokerTokenV1)
	if replay.Code != http.StatusOK || replay.Body.String() != first.Body.String() {
		t.Fatalf("create replay=%d %s want %s", replay.Code, replay.Body.String(), first.Body.String())
	}
	changed := create
	changed.CallbackToken = "changed-callback-token"
	conflict := runnerRequestV1(t, handler, http.MethodPost, "/v1/sessions", changed, testBrokerTokenV1)
	if conflict.Code != http.StatusConflict || !bytes.Contains(conflict.Body.Bytes(), []byte(ErrSessionIDReuseV1.Error())) {
		t.Fatalf("changed create=%d %s", conflict.Code, conflict.Body.String())
	}
	rejectedEvent := testEventV1(created.RunnerSessionID, 1, "session.heartbeat", "active", 0, "")
	if err := store.Advance(create.SessionID, rejectedEvent); err != nil {
		t.Fatal(err)
	}
	rejectedAt := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	if _, err := store.ReserveCallbackAttempt(create.SessionID, rejectedEvent.EventID, rejectedAt, time.Millisecond, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := store.ParkCallbackRejection(create.SessionID, rejectedEvent.EventID, http.StatusConflict, "http_409", rejectedAt); err != nil {
		t.Fatal(err)
	}

	status := runnerRequestV1(t, handler, http.MethodGet, "/v1/sessions/"+created.RunnerSessionID, nil, testBrokerTokenV1)
	if status.Code != http.StatusOK {
		t.Fatalf("status=%d %s", status.Code, status.Body.String())
	}
	var gotStatus RunnerStatusV1
	if err := json.Unmarshal(status.Body.Bytes(), &gotStatus); err != nil || gotStatus.State != "active" || gotStatus.Usage.Unit != WorkUnitV1 || gotStatus.OutputState != OutputStateWaitingV1 || gotStatus.CallbackRejectedTotal != 1 || gotStatus.LastCallbackRejection == nil || gotStatus.LastCallbackRejection.StatusCode != http.StatusConflict {
		t.Fatalf("status=%+v err=%v", gotStatus, err)
	}
	publicStatus := runnerRequestV1(t, handler, http.MethodGet, "/v1/public/sessions/"+created.RunnerSessionID+"/status", nil, "")
	if publicStatus.Code != http.StatusOK || publicStatus.Body.String() != status.Body.String() {
		t.Fatalf("public status=%d %s", publicStatus.Code, publicStatus.Body.String())
	}

	terminated := runnerRequestV1(t, handler, http.MethodDelete, "/v1/sessions/"+created.RunnerSessionID, TerminateRequestV1{Reason: "gateway_close"}, testBrokerTokenV1)
	if terminated.Code != http.StatusOK {
		t.Fatalf("terminate=%d %s", terminated.Code, terminated.Body.String())
	}
	repeated := runnerRequestV1(t, handler, http.MethodDelete, "/v1/sessions/"+created.RunnerSessionID, TerminateRequestV1{Reason: "gateway_close"}, testBrokerTokenV1)
	if repeated.Code != http.StatusOK || repeated.Body.String() != terminated.Body.String() {
		t.Fatalf("terminate replay=%d %s", repeated.Code, repeated.Body.String())
	}
	record, _, err := store.Load(create.SessionID)
	if err != nil || record.State != "ended" || len(record.PendingEvents) != 1 || record.PendingEvents[0].EventType != "session.ended" {
		t.Fatalf("terminal record=%+v err=%v", record, err)
	}
	runtime.mu.Lock()
	ensureCount, terminateCount := runtime.ensureCount, runtime.terminateCount
	runtime.mu.Unlock()
	if ensureCount != 2 || terminateCount != 1 {
		t.Fatalf("runtime calls ensure=%d terminate=%d", ensureCount, terminateCount)
	}
}

func TestLiveRunnerStreamKeyGrantAndRotation(t *testing.T) {
	handler, _, runtime := testLiveRunnerHandlerV1(t)
	create := readStrictFixtureV1[RunnerCreateRequestV1](t, "create-request.json")
	createdRecorder := runnerRequestV1(t, handler, http.MethodPost, "/v1/sessions", create, testBrokerTokenV1)
	var created RunnerCreateResponseV1
	_ = json.Unmarshal(createdRecorder.Body.Bytes(), &created)
	grant := created.Runtime.Grants[0]
	keyPath := "/v1/sessions/" + created.RunnerSessionID + "/stream-keys"
	issue := StreamKeyIssueRequestV1{RequestID: "key_issue_001", Audience: "gateway-relay"}
	first := runnerRequestV1(t, handler, http.MethodPost, keyPath, issue, grant.Secret)
	if first.Code != http.StatusOK {
		t.Fatalf("key issue=%d %s", first.Code, first.Body.String())
	}
	var issued StreamKeyIssueResponseV1
	_ = json.Unmarshal(first.Body.Bytes(), &issued)
	streamPath, token, ok := ParsePrivateIngestStreamKeyV1(issued.StreamKey)
	if !ok || streamPath != "ingest/"+created.RunnerSessionID || token == "" {
		t.Fatalf("issued stream key=%q", issued.StreamKey)
	}
	replay := runnerRequestV1(t, handler, http.MethodPost, keyPath, issue, grant.Secret)
	if replay.Code != http.StatusOK || replay.Body.String() != first.Body.String() {
		t.Fatalf("key replay=%d %s", replay.Code, replay.Body.String())
	}
	wrongAudience := StreamKeyIssueRequestV1{RequestID: "key_issue_wrong", Audience: "direct-publisher"}
	if response := runnerRequestV1(t, handler, http.MethodPost, keyPath, wrongAudience, grant.Secret); response.Code != http.StatusBadRequest {
		t.Fatalf("wrong audience=%d %s", response.Code, response.Body.String())
	}
	rotation := StreamKeyIssueRequestV1{RequestID: "key_issue_002", Audience: "gateway-relay"}
	if response := runnerRequestV1(t, handler, http.MethodPost, keyPath, rotation, grant.Secret); response.Code != http.StatusOK {
		t.Fatalf("rotation=%d %s", response.Code, response.Body.String())
	}
	if response := runnerRequestV1(t, handler, http.MethodPost, keyPath, rotation, grant.Secret); response.Code != http.StatusOK {
		t.Fatalf("rotation replay=%d %s", response.Code, response.Body.String())
	}
	if response := runnerRequestV1(t, handler, http.MethodPost, keyPath, issue, grant.Secret); response.Code != http.StatusConflict || !bytes.Contains(response.Body.Bytes(), []byte(ErrRequestSupersededV1.Error())) {
		t.Fatalf("superseded replay=%d %s", response.Code, response.Body.String())
	}
	if response := runnerRequestV1(t, handler, http.MethodPost, keyPath, rotation, "wrong-grant"); response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong grant=%d %s", response.Code, response.Body.String())
	}
	runtime.mu.Lock()
	activationCount := runtime.activationCount
	runtime.mu.Unlock()
	if activationCount != 1 {
		t.Fatalf("completed rotation activation count=%d", activationCount)
	}
}

func TestLiveRunnerRecoversPendingKeyActivation(t *testing.T) {
	handler, store, runtime := testLiveRunnerHandlerV1(t)
	create := readStrictFixtureV1[RunnerCreateRequestV1](t, "create-request.json")
	createdRecorder := runnerRequestV1(t, handler, http.MethodPost, "/v1/sessions", create, testBrokerTokenV1)
	var created RunnerCreateResponseV1
	_ = json.Unmarshal(createdRecorder.Body.Bytes(), &created)
	grant := created.Runtime.Grants[0]
	path := "/v1/sessions/" + created.RunnerSessionID + "/stream-keys"
	first := StreamKeyIssueRequestV1{RequestID: "key_issue_001", Audience: "gateway-relay"}
	if response := runnerRequestV1(t, handler, http.MethodPost, path, first, grant.Secret); response.Code != http.StatusOK {
		t.Fatalf("first issue=%d %s", response.Code, response.Body.String())
	}
	runtime.failActivation = true
	rotation := StreamKeyIssueRequestV1{RequestID: "key_issue_002", Audience: "gateway-relay"}
	failed := runnerRequestV1(t, handler, http.MethodPost, path, rotation, grant.Secret)
	if failed.Code != http.StatusServiceUnavailable || bytes.Contains(failed.Body.Bytes(), []byte("unsafe activation detail")) {
		t.Fatalf("failed activation=%d %s", failed.Code, failed.Body.String())
	}
	record, _, err := store.Load(create.SessionID)
	if err != nil || record.PendingKeyActivationID != rotation.RequestID {
		t.Fatalf("pending activation=%q err=%v", record.PendingKeyActivationID, err)
	}
	competing := StreamKeyIssueRequestV1{RequestID: "key_issue_003", Audience: "gateway-relay"}
	if response := runnerRequestV1(t, handler, http.MethodPost, path, competing, grant.Secret); response.Code != http.StatusConflict || !bytes.Contains(response.Body.Bytes(), []byte(ErrKeyActivationInFlightV1.Error())) {
		t.Fatalf("competing activation=%d %s", response.Code, response.Body.String())
	}
	runtime.failActivation = false
	if response := runnerRequestV1(t, handler, http.MethodPost, path, rotation, grant.Secret); response.Code != http.StatusOK {
		t.Fatalf("activation recovery=%d %s", response.Code, response.Body.String())
	}
	if response := runnerRequestV1(t, handler, http.MethodPost, path, rotation, grant.Secret); response.Code != http.StatusOK {
		t.Fatalf("completed activation replay=%d %s", response.Code, response.Body.String())
	}
	record, _, err = store.Load(create.SessionID)
	if err != nil || record.PendingKeyActivationID != "" {
		t.Fatalf("completed activation remained pending=%q err=%v", record.PendingKeyActivationID, err)
	}
	runtime.mu.Lock()
	activationCount := runtime.activationCount
	runtime.mu.Unlock()
	if activationCount != 2 {
		t.Fatalf("activation attempts=%d", activationCount)
	}
}

func TestLiveRunnerCreatePersistsBeforeRuntimeAndRecovers(t *testing.T) {
	handler, store, runtime := testLiveRunnerHandlerV1(t)
	runtime.failEnsure = true
	create := readStrictFixtureV1[RunnerCreateRequestV1](t, "create-request.json")
	first := runnerRequestV1(t, handler, http.MethodPost, "/v1/sessions", create, testBrokerTokenV1)
	if first.Code != http.StatusServiceUnavailable || bytes.Contains(first.Body.Bytes(), []byte("unsafe runtime detail")) {
		t.Fatalf("failed runtime create=%d %s", first.Code, first.Body.String())
	}
	record, secrets, err := store.Load(create.SessionID)
	if err != nil || secrets == nil || record.State != "active" {
		t.Fatalf("pre-runtime durable state=%+v secrets=%v err=%v", record, secrets != nil, err)
	}
	runtime.failEnsure = false
	retry := runnerRequestV1(t, handler, http.MethodPost, "/v1/sessions", create, testBrokerTokenV1)
	if retry.Code != http.StatusOK {
		t.Fatalf("recovered create=%d %s", retry.Code, retry.Body.String())
	}
	var recovered RunnerCreateResponseV1
	_ = json.Unmarshal(retry.Body.Bytes(), &recovered)
	if recovered.RunnerSessionID != record.RunnerSessionID {
		t.Fatalf("recovery changed runner ID: %q != %q", recovered.RunnerSessionID, record.RunnerSessionID)
	}
}

func TestLiveRunnerRejectsRuntimeParametersBeforePersistence(t *testing.T) {
	handler, store, runtime := testLiveRunnerHandlerV1(t)
	runtime.failValidation = true
	create := readStrictFixtureV1[RunnerCreateRequestV1](t, "create-request.json")
	response := runnerRequestV1(t, handler, http.MethodPost, "/v1/sessions", create, testBrokerTokenV1)
	if response.Code != http.StatusBadRequest || bytes.Contains(response.Body.Bytes(), []byte("unsafe validation detail")) {
		t.Fatalf("invalid runtime parameters=%d %s", response.Code, response.Body.String())
	}
	if _, _, err := store.Load(create.SessionID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid runtime parameters were persisted: %v", err)
	}
}

func TestLiveRunnerTerminationIntentSurvivesRuntimeFailure(t *testing.T) {
	handler, store, runtime := testLiveRunnerHandlerV1(t)
	create := readStrictFixtureV1[RunnerCreateRequestV1](t, "create-request.json")
	createdRecorder := runnerRequestV1(t, handler, http.MethodPost, "/v1/sessions", create, testBrokerTokenV1)
	var created RunnerCreateResponseV1
	_ = json.Unmarshal(createdRecorder.Body.Bytes(), &created)
	runtime.failTerminate = true
	failed := runnerRequestV1(t, handler, http.MethodDelete, "/v1/sessions/"+created.RunnerSessionID, TerminateRequestV1{Reason: "gateway_close"}, testBrokerTokenV1)
	if failed.Code != http.StatusServiceUnavailable || bytes.Contains(failed.Body.Bytes(), []byte("unsafe runtime detail")) {
		t.Fatalf("failed termination=%d %s", failed.Code, failed.Body.String())
	}
	record, _, err := store.Load(create.SessionID)
	if err != nil || !record.Stopping || record.PendingCloseReason != "gateway_close" || record.State != "active" {
		t.Fatalf("durable stopping intent=%+v err=%v", record, err)
	}
	if replay := runnerRequestV1(t, handler, http.MethodPost, "/v1/sessions", create, testBrokerTokenV1); replay.Code != http.StatusGone {
		t.Fatalf("create restarted stopping session: %d %s", replay.Code, replay.Body.String())
	}
	runtime.failTerminate = false
	recovered := runnerRequestV1(t, handler, http.MethodDelete, "/v1/sessions/"+created.RunnerSessionID, TerminateRequestV1{Reason: "runner_failed"}, testBrokerTokenV1)
	if recovered.Code != http.StatusOK {
		t.Fatalf("termination recovery=%d %s", recovered.Code, recovered.Body.String())
	}
	var response TerminateResponseV1
	_ = json.Unmarshal(recovered.Body.Bytes(), &response)
	if response.CloseReason != "gateway_close" {
		t.Fatalf("termination recovery changed first durable reason: %+v", response)
	}
}

func TestLiveRunnerContractReadyAndUnknownTermination(t *testing.T) {
	handler, _, runtime := testLiveRunnerHandlerV1(t)
	contractResponse := runnerRequestV1(t, handler, http.MethodGet, "/.well-known/livepeer-runner", nil, "")
	var value LiveRunnerContractDocumentV1
	if contractResponse.Code != http.StatusOK || json.Unmarshal(contractResponse.Body.Bytes(), &value) != nil || ValidateRunnerContractV1(value) != nil {
		t.Fatalf("contract=%d %s", contractResponse.Code, contractResponse.Body.String())
	}
	if describe := runnerRequestV1(t, handler, http.MethodGet, "/v1/describe", nil, ""); describe.Code != http.StatusNotFound {
		t.Fatalf("deleted describe route status=%d", describe.Code)
	}
	if ready := runnerRequestV1(t, handler, http.MethodGet, "/ready", nil, ""); ready.Code != http.StatusNoContent {
		t.Fatalf("ready=%d", ready.Code)
	}
	unreadyServer := testLiveRunnerServerV1(t, runtime)
	unreadyServer.Ready = func() bool { return false }
	unreadyHandler, err := unreadyServer.Handler(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) }))
	if err != nil {
		t.Fatal(err)
	}
	if ready := runnerRequestV1(t, unreadyHandler, http.MethodGet, "/ready", nil, ""); ready.Code != http.StatusServiceUnavailable || !bytes.Contains(ready.Body.Bytes(), []byte("media_router_unavailable")) {
		t.Fatalf("unready=%d %s", ready.Code, ready.Body.String())
	}
	unknown := runnerRequestV1(t, handler, http.MethodDelete, "/v1/sessions/unknown_runner", TerminateRequestV1{Reason: "gateway_close"}, testBrokerTokenV1)
	if unknown.Code != http.StatusNoContent {
		t.Fatalf("unknown terminate=%d %s", unknown.Code, unknown.Body.String())
	}
}

func TestRunnerResponseUsesOnlyModulesPublicOrigins(t *testing.T) {
	factory := RunnerResponseFactoryV1{
		PublicRTMPBase: "rtmps://media.example:1936",
		PublicHTTPBase: "https://media.example/r/live-service",
		GrantTTL:       time.Hour,
		Random:         func(size int) ([]byte, error) { return bytes.Repeat([]byte{1}, size), nil },
	}
	response, err := factory.Create(RunnerCreateRequestV1{})
	if err != nil {
		t.Fatal(err)
	}
	if response.Runtime.Public.RTMPURL != "rtmps://media.example:1936/ingest" {
		t.Fatalf("rtmp_url=%q", response.Runtime.Public.RTMPURL)
	}
	prefix := "https://media.example/r/live-service/"
	for name, value := range map[string]string{"hls": response.Runtime.Public.HLSURL, "key": response.Runtime.Public.KeyIssueURL, "status": response.Runtime.Public.StatusURL} {
		if !strings.HasPrefix(value, prefix) {
			t.Fatalf("%s URL escaped public origin: %q", name, value)
		}
	}
}

func TestLiveRunnerProductionMuxSeparatesStatusAndHLSHeadRoutes(t *testing.T) {
	server := testLiveRunnerServerV1(t, &fakeLiveRuntimeV1{})
	type routedRequest struct{ method, id, rendition, asset string }
	var routed []routedRequest
	server.HLS = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		routed = append(routed, routedRequest{request.Method, request.PathValue("id"), request.PathValue("rendition"), request.PathValue("asset")})
		writer.WriteHeader(http.StatusNoContent)
	})
	handler, err := server.Handler(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) }))
	if err != nil {
		t.Fatal(err)
	}
	master := runnerRequestV1(t, handler, http.MethodGet, "/v1/public/sessions/runner_mux/master.m3u8", nil, "")
	asset := runnerRequestV1(t, handler, http.MethodHead, "/v1/public/sessions/runner_mux/720p/index.m3u8", nil, "")
	statusHead := runnerRequestV1(t, handler, http.MethodHead, "/v1/public/sessions/runner_mux/status", nil, "")
	if master.Code != http.StatusNoContent || asset.Code != http.StatusNoContent || statusHead.Code != http.StatusMethodNotAllowed || statusHead.Header().Get("Allow") != "GET" {
		t.Fatalf("master=%d asset=%d status HEAD=%d allow=%q", master.Code, asset.Code, statusHead.Code, statusHead.Header().Get("Allow"))
	}
	if len(routed) != 2 || routed[0] != (routedRequest{http.MethodGet, "runner_mux", "", ""}) || routed[1] != (routedRequest{http.MethodHead, "runner_mux", "720p", "index.m3u8"}) {
		t.Fatalf("HLS routes=%+v", routed)
	}
	for _, path := range []string{"/v1/public/sessions/runner_mux/master.m3u8", "/v1/public/sessions/runner_mux/720p/index.m3u8"} {
		if got := runnerRequestV1(t, handler, http.MethodOptions, path, nil, ""); got.Code != http.StatusNoContent {
			t.Fatalf("HLS preflight route: %d", got.Code)
		}
	}
	if got := runnerRequestV1(t, handler, http.MethodOptions, "/v1/sessions/runner_mux", nil, ""); got.Code == http.StatusNoContent {
		t.Fatal("management inherited public preflight")
	}

}

func testLiveRunnerHandlerV1(t *testing.T) (http.Handler, *EncryptedFileSessionStoreV1, *fakeLiveRuntimeV1) {
	t.Helper()
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x66}, 32))
	runtime := &fakeLiveRuntimeV1{}
	server := testLiveRunnerServerWithStoreV1(t, store, runtime)
	handler, err := server.Handler(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) }))
	if err != nil {
		t.Fatal(err)
	}
	return handler, store, runtime
}

func testLiveRunnerServerV1(t *testing.T, runtime LiveSessionRuntimeV1) *LiveRunnerServerV1 {
	t.Helper()
	return testLiveRunnerServerWithStoreV1(t, newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x67}, 32)), runtime)
}

func testLiveRunnerServerWithStoreV1(t *testing.T, store *EncryptedFileSessionStoreV1, runtime LiveSessionRuntimeV1) *LiveRunnerServerV1 {
	t.Helper()
	fixedNow := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	var randomByte byte
	randomSource := func(size int) ([]byte, error) {
		randomByte++
		return bytes.Repeat([]byte{randomByte}, size), nil
	}
	return &LiveRunnerServerV1{
		Store: store, Runtime: runtime, BrokerToken: testBrokerTokenV1, KeyTTL: 10 * time.Minute, Now: func() time.Time { return fixedNow }, Random: randomSource,
		Factory: RunnerResponseFactoryV1{
			PublicRTMPBase: "rtmps://runner.example:1936", PublicHTTPBase: "https://runner.example/r/live-runner", GrantTTL: time.Hour,
			Now: func() time.Time { return fixedNow }, Random: randomSource,
		},
	}
}

func runnerRequestV1(t *testing.T, handler http.Handler, method, path string, body any, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// Under the attach model the broker reaches the session routes over the
// pool member agent's tunnel and presents no bearer. With no
// LIVE_RUNNER_BROKER_TOKEN configured the routes must admit that request;
// the first eu-central certification failed here with a 401 on create.
func TestLiveRunnerAdmitsUnauthenticatedCreateWithoutBrokerToken(t *testing.T) {
	store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{0x68}, 32))
	runtime := &fakeLiveRuntimeV1{}
	server := testLiveRunnerServerWithStoreV1(t, store, runtime)
	server.BrokerToken = ""
	handler, err := server.Handler(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) }))
	if err != nil {
		t.Fatal(err)
	}
	create := readStrictFixtureV1[RunnerCreateRequestV1](t, "create-request.json")
	created := runnerRequestV1(t, handler, http.MethodPost, "/v1/sessions", create, "")
	if created.Code == http.StatusUnauthorized {
		t.Fatalf("create without a bearer was refused although no broker token is configured: %d %s", created.Code, created.Body.String())
	}
	if created.Code != http.StatusCreated && created.Code != http.StatusOK {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
}

func TestBrokerAndHistoricalReasonsCanFinishPinnedWinddown(t *testing.T) {
	for _, reason := range []string{"lease_exhausted", "customer_end", "publisher_disconnect", "broker_ended", "relay_failure", "refill_preannounced_refusal", "refill_policy_exhausted", "refill_total_exceeded", "runner_ended", "insufficient_balance", "authorization_exhausted", "open_failed", "capacity_exhausted"} {
		t.Run(reason, func(t *testing.T) {
			handler, _, runtime := testLiveRunnerHandlerV1(t)
			create := readStrictFixtureV1[RunnerCreateRequestV1](t, "create-request.json")
			response := runnerRequestV1(t, handler, http.MethodPost, "/v1/sessions", create, testBrokerTokenV1)
			if response.Code != http.StatusOK {
				t.Fatalf("create=%d", response.Code)
			}
			var created RunnerCreateResponseV1
			if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			terminated := runnerRequestV1(t, handler, http.MethodDelete, "/v1/sessions/"+created.RunnerSessionID, TerminateRequestV1{Reason: reason}, testBrokerTokenV1)
			if terminated.Code != http.StatusOK {
				t.Fatalf("terminate=%d %s", terminated.Code, terminated.Body.String())
			}
			var ended TerminateResponseV1
			if err := json.Unmarshal(terminated.Body.Bytes(), &ended); err != nil {
				t.Fatal(err)
			}
			if ended.CloseReason != reason {
				t.Fatalf("lost pinned reason: %q", ended.CloseReason)
			}
			replay := runnerRequestV1(t, handler, http.MethodDelete, "/v1/sessions/"+created.RunnerSessionID, TerminateRequestV1{Reason: reason}, testBrokerTokenV1)
			if replay.Code != http.StatusOK || replay.Body.String() != terminated.Body.String() {
				t.Fatal("termination replay drift")
			}
			if runtime.terminateCount != 1 {
				t.Fatalf("terminate calls=%d", runtime.terminateCount)
			}
		})
	}
}
