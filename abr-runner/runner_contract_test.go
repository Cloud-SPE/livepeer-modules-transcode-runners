package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestABRRunnerContractDeclaresTrailerMeteredStream(t *testing.T) {
	response := httptest.NewRecorder()
	handleRunnerContractV2(response, httptest.NewRequest(http.MethodGet, "/.well-known/livepeer-runner", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	var contract map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &contract); err != nil {
		t.Fatal(err)
	}
	if contract["capability_id"] != "video:transcode.abr" || contract["protocol"] != "paid-job/v1" {
		t.Fatalf("contract=%v", contract)
	}
	workUnit := contract["work_unit"].(map[string]any)
	if workUnit["name"] != ABRWorkUnitV2 {
		t.Fatalf("work_unit=%v", workUnit)
	}
	extractor := workUnit["extractor"].(map[string]any)
	if extractor["type"] != "response-trailer" || extractor["trailer"] != ABRWorkUnitsTrailerV2 {
		t.Fatalf("extractor=%v", extractor)
	}
}

func TestABRRunnerContractIsGetOnly(t *testing.T) {
	response := httptest.NewRecorder()
	handleRunnerContractV2(response, httptest.NewRequest(http.MethodPost, "/.well-known/livepeer-runner", nil))
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("response=%d headers=%v", response.Code, response.Header())
	}
}
