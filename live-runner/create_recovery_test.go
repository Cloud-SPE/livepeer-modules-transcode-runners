package liverunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

func TestCreateReconciliationFenceSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{9}, 32)
	store := newTestStoreV1(t, dir, key)
	req, resp := testCreatePairV1(t)
	result, err := store.ReconcileCreate(req.SessionID)
	if err != nil || result.Outcome != "fenced" || result.SessionID != req.SessionID || result.RunnerSessionID != "" {
		t.Fatalf("fence=%+v %v", result, err)
	}
	store = newTestStoreV1(t, dir, key)
	if _, _, _, err = store.CreateOrReplay(req, resp); !errors.Is(err, ErrCreateFencedV1) {
		t.Fatalf("late create=%v", err)
	}
	again, err := store.ReconcileCreate(req.SessionID)
	if err != nil || again != result {
		t.Fatalf("replay=%+v %v", again, err)
	}
	if err = os.WriteFile(filepath.Join(dir, req.SessionID+".create-fence"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ReconcileCreate(req.SessionID); err == nil {
		t.Fatal("accepted corrupt fence")
	}
	if _, _, _, err = store.CreateOrReplay(req, resp); err == nil {
		t.Fatal("created over corrupt fence")
	}
}
func TestCreateReconciliationRace(t *testing.T) {
	for i := 0; i < 30; i++ {
		store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{7}, 32))
		req, resp := testCreatePairV1(t)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var createErr, reconcileErr error
		var result CreateResolutionV1
		go func() { defer wg.Done(); <-start; _, _, _, createErr = store.CreateOrReplay(req, resp) }()
		go func() { defer wg.Done(); <-start; result, reconcileErr = store.ReconcileCreate(req.SessionID) }()
		close(start)
		wg.Wait()
		if reconcileErr != nil {
			t.Fatal(reconcileErr)
		}
		if result.Outcome == "created" {
			if createErr != nil || result.RunnerSessionID != resp.RunnerSessionID {
				t.Fatalf("create=%v result=%+v", createErr, result)
			}
		} else if result.Outcome == "fenced" {
			if !errors.Is(createErr, ErrCreateFencedV1) {
				t.Fatalf("fenced but create=%v", createErr)
			}
		} else {
			t.Fatal(result)
		}
	}
}
func TestReconcileHTTPExistingAndAbsent(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "lost-create-response"}[exists], func(t *testing.T) {
			handler, store, runtime := testLiveRunnerHandlerV1(t)
			req := readStrictFixtureV1[RunnerCreateRequestV1](t, "create-request.json")
			body := map[string]string{"session_id": req.SessionID}
			denied := runnerRequestV1(t, handler, http.MethodPost, "/v1/session-creates/reconcile", body, "")
			if denied.Code != 401 {
				t.Fatalf("unauthorized=%d", denied.Code)
			}
			if exists {
				created := runnerRequestV1(t, handler, http.MethodPost, "/v1/sessions", req, testBrokerTokenV1)
				if created.Code != 200 {
					t.Fatal(created.Body.String())
				}
			}
			resolved := runnerRequestV1(t, handler, http.MethodPost, "/v1/session-creates/reconcile", body, testBrokerTokenV1)
			var result CreateResolutionV1
			if resolved.Code != 200 || json.Unmarshal(resolved.Body.Bytes(), &result) != nil {
				t.Fatalf("resolve=%d %s", resolved.Code, resolved.Body.String())
			}
			if exists {
				if result.Outcome != "created" || result.RunnerSessionID == "" {
					t.Fatal(result)
				}
				ended := runnerRequestV1(t, handler, http.MethodDelete, "/v1/sessions/"+result.RunnerSessionID, TerminateRequestV1{Reason: "open_failed"}, testBrokerTokenV1)
				if ended.Code != 200 {
					t.Fatalf("terminate=%d %s", ended.Code, ended.Body.String())
				}
				record, _, err := store.Load(req.SessionID)
				if err != nil || record.State == "active" {
					t.Fatalf("still active: %+v %v", record, err)
				}
			} else if result.Outcome != "fenced" {
				t.Fatal(result)
			}
			before := runtime.ensureCount
			replay := runnerRequestV1(t, handler, http.MethodPost, "/v1/sessions", req, testBrokerTokenV1)
			if replay.Code != 410 || runtime.ensureCount != before {
				t.Fatalf("late create=%d ensure=%d", replay.Code, runtime.ensureCount)
			}
		})
	}
}
func TestCapacityRefusalCannotLaterCreate(t *testing.T) {
	handler, _, runtime := testLiveRunnerHandlerV1(t)
	req := readStrictFixtureV1[RunnerCreateRequestV1](t, "create-request.json")
	runtime.ensureErr = transcode.ErrGPUAdmissionCapacity
	response := runnerRequestV1(t, handler, http.MethodPost, "/v1/sessions", req, testBrokerTokenV1)
	if response.Code != 429 {
		t.Fatalf("refusal=%d %s", response.Code, response.Body.String())
	}
	runtime.ensureErr = nil
	response = runnerRequestV1(t, handler, http.MethodPost, "/v1/sessions", req, testBrokerTokenV1)
	if response.Code != 410 || runtime.ensureCount != 1 {
		t.Fatalf("late replay=%d starts=%d", response.Code, runtime.ensureCount)
	}
}

type blockingCreateRuntimeV1 struct {
	*fakeLiveRuntimeV1
	entered, release chan struct{}
}

func (r *blockingCreateRuntimeV1) EnsureSession(ctx context.Context, record SessionRecordV1, secrets SessionSecretsV1) error {
	close(r.entered)
	<-r.release
	return r.fakeLiveRuntimeV1.EnsureSession(ctx, record, secrets)
}
func TestReconcileWaitsForRunningCreate(t *testing.T) {
	runtime := &blockingCreateRuntimeV1{fakeLiveRuntimeV1: &fakeLiveRuntimeV1{}, entered: make(chan struct{}), release: make(chan struct{})}
	server := testLiveRunnerServerV1(t, runtime)
	handler, err := server.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	if err != nil {
		t.Fatal(err)
	}
	req := readStrictFixtureV1[RunnerCreateRequestV1](t, "create-request.json")
	created := make(chan int, 1)
	resolved := make(chan int, 1)
	go func() { created <- runnerRequestV1(t, handler, "POST", "/v1/sessions", req, testBrokerTokenV1).Code }()
	<-runtime.entered
	go func() {
		resolved <- runnerRequestV1(t, handler, "POST", "/v1/session-creates/reconcile", map[string]string{"session_id": req.SessionID}, testBrokerTokenV1).Code
	}()
	select {
	case code := <-resolved:
		close(runtime.release)
		t.Fatalf("reconciled executing create: %d", code)
	case <-time.After(20 * time.Millisecond):
	}
	close(runtime.release)
	if code := <-created; code != 200 {
		t.Fatalf("create=%d", code)
	}
	if code := <-resolved; code != 200 {
		t.Fatalf("reconcile=%d", code)
	}
}

func TestCreateRecoveryContractFixtures(t *testing.T) {
	req := readStrictFixtureV1[struct {
		SessionID string `json:"session_id"`
	}](t, "create-reconcile-request.json")
	for _, exists := range []bool{false, true} {
		store := newTestStoreV1(t, t.TempDir(), bytes.Repeat([]byte{8}, 32))
		fixture := "create-reconcile-fenced.json"
		if exists {
			create, response := testCreatePairV1(t)
			if _, _, _, err := store.CreateOrReplay(create, response); err != nil {
				t.Fatal(err)
			}
			fixture = "create-reconcile-created.json"
		}
		want := readStrictFixtureV1[CreateResolutionV1](t, fixture)
		got, err := store.ReconcileCreate(req.SessionID)
		if err != nil || got != want {
			t.Fatalf("fixture %s: %+v want %+v err=%v", fixture, got, want, err)
		}
	}
}
