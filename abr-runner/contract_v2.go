package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

const (
	ABRRequestSchemaV2    = "video-transcode-abr/v2"
	ABRProgressSchemaV2   = "video-transcode-abr-progress/v2"
	ABRResultSchemaV2     = "video-transcode-abr-result/v2"
	ABRWorkUnitV2         = "video-frame-megapixel"
	ABRWorkUnitsTrailerV2 = "X-Livepeer-Work-Units"
)

var (
	workloadIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	sha256Pattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

var progressPhasesV2 = map[string]struct{}{
	"accepted":    {},
	"downloading": {},
	"probing":     {},
	"encoding":    {},
	"packaging":   {},
	"uploading":   {},
}

// ABRWorkloadRequestV2 is the runner-owned body carried inside paid-job/v1.
// Download and upload URLs are credentials: callers and runners must redact
// the body from logs and must never echo those URLs in SSE events or results.
type ABRWorkloadRequestV2 struct {
	Schema     string                  `json:"schema"`
	WorkloadID string                  `json:"workload_id"`
	Input      ABRInputV2              `json:"input"`
	Ladder     ABRLadderV2             `json:"ladder"`
	Output     ABROutputDestinationsV2 `json:"output"`
}

type ABRInputV2 struct {
	DownloadURL   string `json:"download_url"`
	ContentSHA256 string `json:"content_sha256,omitempty"`
}

type ABRLadderV2 struct {
	Preset string `json:"preset"`
}

type ArtifactDestinationV2 struct {
	// ArtifactURI is a stable, non-secret storage reference returned on success.
	ArtifactURI string `json:"artifact_uri"`
	// UploadURL is a presigned PUT URL and is never returned by the runner.
	UploadURL string `json:"upload_url"`
}

type ABROutputDestinationsV2 struct {
	Manifest   ArtifactDestinationV2              `json:"manifest"`
	Renditions map[string]RenditionDestinationsV2 `json:"renditions"`
}

type RenditionDestinationsV2 struct {
	Playlist ArtifactDestinationV2 `json:"playlist"`
	Stream   ArtifactDestinationV2 `json:"stream"`
}

type ABRProgressV2 struct {
	Schema          string  `json:"schema"`
	WorkloadID      string  `json:"workload_id"`
	RequestSHA256   string  `json:"request_sha256"`
	Sequence        uint64  `json:"sequence"`
	Phase           string  `json:"phase"`
	OverallProgress float64 `json:"overall_progress"`
	Rendition       string  `json:"rendition,omitempty"`
	Frames          uint64  `json:"frames,omitempty"`
}

type ABRTerminalResultV2 struct {
	Schema        string              `json:"schema"`
	WorkloadID    string              `json:"workload_id"`
	RequestSHA256 string              `json:"request_sha256"`
	Outcome       string              `json:"outcome"`
	ManifestURI   string              `json:"manifest_uri"`
	Renditions    []RenditionResultV2 `json:"renditions"`
	Usage         UsageClaimV2        `json:"usage"`
}

type RenditionResultV2 struct {
	Name          string            `json:"name"`
	PlaylistURI   string            `json:"playlist_uri"`
	StreamURI     string            `json:"stream_uri"`
	Video         *DeliveredVideoV2 `json:"video,omitempty"`
	FileSizeBytes uint64            `json:"file_size_bytes"`
}

type DeliveredVideoV2 struct {
	ActualFrames uint64 `json:"actual_frames"`
	Width        uint32 `json:"width"`
	Height       uint32 `json:"height"`
}

type UsageClaimV2 struct {
	Unit  string `json:"unit"`
	Units uint64 `json:"units"`
}

type ABRTerminalErrorV2 struct {
	Schema        string       `json:"schema"`
	WorkloadID    string       `json:"workload_id"`
	RequestSHA256 string       `json:"request_sha256"`
	Outcome       string       `json:"outcome"`
	Error         ABRErrorV2   `json:"error"`
	Usage         UsageClaimV2 `json:"usage"`
}

type ABRErrorV2 struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type WorkloadReplayDispositionV2 string

const (
	ResumeExistingV2 WorkloadReplayDispositionV2 = "resume_existing"
	ReplayTerminalV2 WorkloadReplayDispositionV2 = "replay_terminal"
	RejectIDReuseV2  WorkloadReplayDispositionV2 = "reject_workload_id_reuse"
)

// ClassifyWorkloadReplayV2 makes runner idempotency independent of broker
// behavior. A matching request never launches a second FFmpeg execution.
func ClassifyWorkloadReplayV2(existingSHA256, incomingSHA256 string, terminal bool) WorkloadReplayDispositionV2 {
	if existingSHA256 != incomingSHA256 {
		return RejectIDReuseV2
	}
	if terminal {
		return ReplayTerminalV2
	}
	return ResumeExistingV2
}

// ValidateABRRequestV2 validates the transport-independent workload shape.
// requiredRenditions is the selected preset's exact rendition-name set.
func ValidateABRRequestV2(req ABRWorkloadRequestV2, requiredRenditions []string) error {
	if req.Schema != ABRRequestSchemaV2 {
		return fmt.Errorf("schema must be %q", ABRRequestSchemaV2)
	}
	if !workloadIDPattern.MatchString(req.WorkloadID) {
		return errors.New("workload_id must be 1-128 safe opaque characters")
	}
	if err := validateCredentialURL(req.Input.DownloadURL, "input.download_url"); err != nil {
		return err
	}
	if req.Input.ContentSHA256 != "" && !sha256Pattern.MatchString(req.Input.ContentSHA256) {
		return errors.New("input.content_sha256 must be lowercase SHA-256 hex")
	}
	if strings.TrimSpace(req.Ladder.Preset) == "" {
		return errors.New("ladder.preset is required")
	}
	if err := validateDestination(req.Output.Manifest, "output.manifest"); err != nil {
		return err
	}
	if len(requiredRenditions) == 0 {
		return errors.New("selected preset must contain at least one rendition")
	}
	required := make(map[string]struct{}, len(requiredRenditions))
	for _, name := range requiredRenditions {
		if _, duplicate := required[name]; duplicate || strings.TrimSpace(name) == "" {
			return errors.New("selected preset contains an invalid or duplicate rendition name")
		}
		required[name] = struct{}{}
		outputs, ok := req.Output.Renditions[name]
		if !ok {
			return fmt.Errorf("output.renditions is missing %q", name)
		}
		if err := validateDestination(outputs.Playlist, "output.renditions."+name+".playlist"); err != nil {
			return err
		}
		if err := validateDestination(outputs.Stream, "output.renditions."+name+".stream"); err != nil {
			return err
		}
	}
	for name := range req.Output.Renditions {
		if _, ok := required[name]; !ok {
			return fmt.Errorf("output.renditions contains unexpected rendition %q", name)
		}
	}
	return nil
}

func RequestContentSHA256V2(req ABRWorkloadRequestV2) (string, error) {
	var canonical bytes.Buffer
	encoder := json.NewEncoder(&canonical)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(req); err != nil {
		return "", fmt.Errorf("marshal canonical request: %w", err)
	}
	body := bytes.TrimSuffix(canonical.Bytes(), []byte("\n"))
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

func ValidateABRProgressV2(progress ABRProgressV2) error {
	if progress.Schema != ABRProgressSchemaV2 {
		return fmt.Errorf("schema must be %q", ABRProgressSchemaV2)
	}
	if !workloadIDPattern.MatchString(progress.WorkloadID) || !sha256Pattern.MatchString(progress.RequestSHA256) {
		return errors.New("progress identity is invalid")
	}
	if progress.Sequence == 0 {
		return errors.New("progress sequence starts at 1")
	}
	if _, ok := progressPhasesV2[progress.Phase]; !ok {
		return errors.New("progress phase is invalid")
	}
	if math.IsNaN(progress.OverallProgress) || math.IsInf(progress.OverallProgress, 0) || progress.OverallProgress < 0 || progress.OverallProgress > 100 {
		return errors.New("progress phase or percentage is invalid")
	}
	return nil
}

func ValidateABRTerminalResultV2(result ABRTerminalResultV2) error {
	if result.Schema != ABRResultSchemaV2 || result.Outcome != "succeeded" {
		return errors.New("terminal result schema or outcome is invalid")
	}
	if !workloadIDPattern.MatchString(result.WorkloadID) || !sha256Pattern.MatchString(result.RequestSHA256) {
		return errors.New("terminal result identity is invalid")
	}
	if err := validateArtifactURI(result.ManifestURI, "manifest_uri"); err != nil {
		return err
	}
	if len(result.Renditions) == 0 {
		return errors.New("terminal result requires manifest and renditions")
	}
	seen := make(map[string]struct{}, len(result.Renditions))
	for _, rendition := range result.Renditions {
		if rendition.Name == "" {
			return errors.New("rendition result is missing artifact identity")
		}
		if err := validateArtifactURI(rendition.PlaylistURI, "rendition.playlist_uri"); err != nil {
			return err
		}
		if err := validateArtifactURI(rendition.StreamURI, "rendition.stream_uri"); err != nil {
			return err
		}
		if _, duplicate := seen[rendition.Name]; duplicate {
			return fmt.Errorf("duplicate rendition result %q", rendition.Name)
		}
		seen[rendition.Name] = struct{}{}
		if rendition.Video != nil && (rendition.Video.ActualFrames == 0 || rendition.Video.Width == 0 || rendition.Video.Height == 0) {
			return fmt.Errorf("video rendition %q has invalid delivered dimensions or frames", rendition.Name)
		}
	}
	units, err := CalculateFrameMegapixelUnitsV2(result.Renditions)
	if err != nil {
		return err
	}
	if result.Usage.Unit != ABRWorkUnitV2 || result.Usage.Units != units {
		return fmt.Errorf("usage claim mismatch: expected %s=%d", ABRWorkUnitV2, units)
	}
	return nil
}

func ValidateABRTerminalErrorV2(result ABRTerminalErrorV2) error {
	if result.Schema != ABRResultSchemaV2 || result.Outcome != "failed" {
		return errors.New("terminal error schema or outcome is invalid")
	}
	if !workloadIDPattern.MatchString(result.WorkloadID) || !sha256Pattern.MatchString(result.RequestSHA256) {
		return errors.New("terminal error identity is invalid")
	}
	if result.Error.Code == "" || result.Error.Message == "" {
		return errors.New("terminal error code and redacted message are required")
	}
	message := strings.ToLower(result.Error.Message)
	if len(result.Error.Message) > 512 || strings.Contains(message, "http://") || strings.Contains(message, "https://") || strings.Contains(message, "sig=") {
		return errors.New("terminal error message is not safely redacted")
	}
	if result.Usage.Unit != ABRWorkUnitV2 {
		return errors.New("failed terminal work unit is invalid")
	}
	return nil
}

// CalculateFrameMegapixelUnitsV2 implements the product billing decision:
// ceil(sum(actual_frames_i * width_i * height_i) / 1_000_000).
// Audio-only renditions have Video == nil and contribute zero.
func CalculateFrameMegapixelUnitsV2(renditions []RenditionResultV2) (uint64, error) {
	var pixels uint64
	for _, rendition := range renditions {
		if rendition.Video == nil {
			continue
		}
		video := rendition.Video
		hi, frameWidth := bits.Mul64(video.ActualFrames, uint64(video.Width))
		if hi != 0 {
			return 0, fmt.Errorf("pixel count overflow for rendition %q", rendition.Name)
		}
		hi, renditionPixels := bits.Mul64(frameWidth, uint64(video.Height))
		if hi != 0 {
			return 0, fmt.Errorf("pixel count overflow for rendition %q", rendition.Name)
		}
		var carry uint64
		pixels, carry = bits.Add64(pixels, renditionPixels, 0)
		if carry != 0 {
			return 0, errors.New("aggregate pixel count overflow")
		}
	}
	units := pixels / 1_000_000
	if pixels%1_000_000 != 0 {
		units++
	}
	return units, nil
}

func CanonicalRenditionNamesV2(names []string) []string {
	result := append([]string(nil), names...)
	sort.Strings(result)
	return result
}

func validateDestination(destination ArtifactDestinationV2, field string) error {
	if err := validateArtifactURI(destination.ArtifactURI, field+".artifact_uri"); err != nil {
		return err
	}
	return validateCredentialURL(destination.UploadURL, field+".upload_url")
}

func validateArtifactURI(raw, field string) error {
	if strings.TrimSpace(raw) == "" || len(raw) > 1024 || strings.ContainsAny(raw, "?\r\n") {
		return fmt.Errorf("%s must be a non-secret stable artifact reference", field)
	}
	return nil
}

func validateCredentialURL(raw, field string) error {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("%s must be an http(s) URL without userinfo", field)
	}
	return nil
}
