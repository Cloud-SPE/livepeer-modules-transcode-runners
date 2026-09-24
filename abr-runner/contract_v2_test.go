package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"strings"
	"testing"
)

const fixtureRequestSHA256 = "6ec51df51598e35eb851973ab08d2904fc3b3eed948fb211fa39b0a20a9bdef0"

func TestV2RequestFixtureAndStableContentHash(t *testing.T) {
	req := readJSONFixture[ABRWorkloadRequestV2](t, "testdata/contracts/v2/request.json")
	required := []string{"720p", "360p", "audio-only"}
	if err := ValidateABRRequestV2(req, required); err != nil {
		t.Fatalf("ValidateABRRequestV2() error = %v", err)
	}
	hash, err := RequestContentSHA256V2(req)
	if err != nil {
		t.Fatal(err)
	}
	if hash != fixtureRequestSHA256 {
		t.Fatalf("request hash = %s, want %s", hash, fixtureRequestSHA256)
	}

	// Map insertion order cannot change the canonical JSON request hash.
	reordered := req
	reordered.Output.Renditions = map[string]RenditionDestinationsV2{
		"audio-only": req.Output.Renditions["audio-only"],
		"360p":       req.Output.Renditions["360p"],
		"720p":       req.Output.Renditions["720p"],
	}
	reorderedHash, err := RequestContentSHA256V2(reordered)
	if err != nil || reorderedHash != hash {
		t.Fatalf("reordered request hash = %s, %v; want %s", reorderedHash, err, hash)
	}
}

func TestV2RequestRejectsOutputDriftAndCredentialUserinfo(t *testing.T) {
	req := readJSONFixture[ABRWorkloadRequestV2](t, "testdata/contracts/v2/request.json")
	delete(req.Output.Renditions, "360p")
	if err := ValidateABRRequestV2(req, []string{"720p", "360p", "audio-only"}); err == nil {
		t.Fatal("missing preset output was accepted")
	}

	req = readJSONFixture[ABRWorkloadRequestV2](t, "testdata/contracts/v2/request.json")
	req.Input.DownloadURL = "https://user:secret@storage.example/input.mp4"
	if err := ValidateABRRequestV2(req, []string{"720p", "360p", "audio-only"}); err == nil {
		t.Fatal("URL userinfo was accepted")
	}
}

func TestV2SSEFixturesValidateAndNeverEchoCredentialURLs(t *testing.T) {
	progressData, progressRaw := readSSEData(t, "testdata/contracts/v2/progress.sse", "progress")
	var progress ABRProgressV2
	decodeStrict(t, progressData, &progress)
	if err := ValidateABRProgressV2(progress); err != nil {
		t.Fatalf("progress fixture: %v", err)
	}
	if !strings.Contains(progressRaw, ": keepalive") {
		t.Fatal("progress fixture must demonstrate an SSE keepalive")
	}

	resultData, resultRaw := readSSEData(t, "testdata/contracts/v2/result.sse", "result")
	var result ABRTerminalResultV2
	decodeStrict(t, resultData, &result)
	if err := ValidateABRTerminalResultV2(result); err != nil {
		t.Fatalf("result fixture: %v", err)
	}
	if result.Usage.Units != 346 {
		t.Fatalf("result units = %d, want 346", result.Usage.Units)
	}
	result.Usage.Units--
	if err := ValidateABRTerminalResultV2(result); err == nil {
		t.Fatal("incorrect terminal usage was accepted")
	}

	errorData, errorRaw := readSSEData(t, "testdata/contracts/v2/error.sse", "error")
	var terminalError ABRTerminalErrorV2
	decodeStrict(t, errorData, &terminalError)
	if err := ValidateABRTerminalErrorV2(terminalError); err != nil {
		t.Fatalf("error fixture: %v", err)
	}
	terminalError.Usage.Unit = "frames"
	if err := ValidateABRTerminalErrorV2(terminalError); err == nil {
		t.Fatal("incorrect failed usage unit was accepted")
	}
	for name, raw := range map[string]string{"progress": progressRaw, "result": resultRaw, "error": errorRaw} {
		if strings.Contains(raw, "sig=") || strings.Contains(raw, "upload_url") || strings.Contains(raw, "download_url") {
			t.Fatalf("%s event leaks a credential URL", name)
		}
	}
}

func TestFrameMegapixelUsageSumsBeforeOneCeilingAndSkipsAudio(t *testing.T) {
	renditions := []RenditionResultV2{
		{Name: "tiny-a", Video: &DeliveredVideoV2{ActualFrames: 100, Width: 1, Height: 1}},
		{Name: "tiny-b", Video: &DeliveredVideoV2{ActualFrames: 100, Width: 1, Height: 1}},
		{Name: "audio-only", Video: nil},
	}
	units, err := CalculateFrameMegapixelUnitsV2(renditions)
	if err != nil {
		t.Fatal(err)
	}
	if units != 1 {
		t.Fatalf("units = %d, want one aggregate ceiling (not one per rendition)", units)
	}
}

func TestFrameMegapixelUsageRejectsOverflow(t *testing.T) {
	_, err := CalculateFrameMegapixelUnitsV2([]RenditionResultV2{{
		Name:  "overflow",
		Video: &DeliveredVideoV2{ActualFrames: math.MaxUint64, Width: 2, Height: 2},
	}})
	if err == nil {
		t.Fatal("overflow was accepted")
	}
}

func TestWorkloadReplayNeverCreatesReplacementExecution(t *testing.T) {
	if got := ClassifyWorkloadReplayV2("same", "same", false); got != ResumeExistingV2 {
		t.Fatalf("in-flight identical replay = %q", got)
	}
	if got := ClassifyWorkloadReplayV2("same", "same", true); got != ReplayTerminalV2 {
		t.Fatalf("terminal identical replay = %q", got)
	}
	if got := ClassifyWorkloadReplayV2("old", "different", false); got != RejectIDReuseV2 {
		t.Fatalf("different-content replay = %q", got)
	}
}

func readJSONFixture[T any](t *testing.T, path string) T {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result T
	decodeStrict(t, body, &result)
	return result
}

func readSSEData(t *testing.T, path, wantEvent string) ([]byte, string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var event, data string
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") && data == "" {
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if event != wantEvent || data == "" {
		t.Fatalf("fixture event = %q, data present = %t; want %q", event, data != "", wantEvent)
	}
	return []byte(data), string(body)
}

func decodeStrict(t *testing.T, body []byte, target any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("fixture contains trailing JSON: %v", err)
	}
}
