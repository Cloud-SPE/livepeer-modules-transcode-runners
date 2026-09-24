package liverunner

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	MediaMTXVersionV1 = "1.20.1"
	MediaMTXImageV1   = "bluenviron/mediamtx:1.20.1@sha256:1b029d11049be75630e9b73bb0d5f47b08a7db4eaee89a80bf8f53bc40e56414"
)

type MediaMTXConfigV1 struct {
	AuthHTTPAddress string
	RTMPAddress     string
	HLSAddress      string
	APIAddress      string
	MetricsAddress  string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	SegmentDuration time.Duration
	PartDuration    time.Duration
	SegmentCount    uint32
}

func DefaultMediaMTXConfigV1(authHTTPAddress string) MediaMTXConfigV1 {
	return MediaMTXConfigV1{
		AuthHTTPAddress: authHTTPAddress,
		RTMPAddress:     ":1935", HLSAddress: "127.0.0.1:8888",
		APIAddress: "127.0.0.1:9997", MetricsAddress: "127.0.0.1:9998",
		ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second,
		SegmentDuration: time.Second, PartDuration: 200 * time.Millisecond, SegmentCount: 7,
	}
}

func RenderMediaMTXConfigV1(value MediaMTXConfigV1) ([]byte, error) {
	if err := validateHTTPURL(value.AuthHTTPAddress); err != nil {
		return nil, errors.New("MediaMTX auth address is invalid")
	}
	if err := validateListenAddressV1(value.RTMPAddress, false); err != nil {
		return nil, fmt.Errorf("MediaMTX RTMP address: %w", err)
	}
	for name, address := range map[string]string{"HLS": value.HLSAddress, "API": value.APIAddress, "metrics": value.MetricsAddress} {
		if err := validateListenAddressV1(address, true); err != nil {
			return nil, fmt.Errorf("MediaMTX %s address: %w", name, err)
		}
	}
	if value.ReadTimeout <= 0 || value.WriteTimeout <= 0 || value.SegmentDuration <= 0 || value.PartDuration <= 0 || value.PartDuration >= value.SegmentDuration || value.SegmentCount < 3 {
		return nil, errors.New("MediaMTX timing configuration is invalid")
	}
	config := fmt.Sprintf(`logLevel: info
logDestinations: [stdout]
logStructured: true
readTimeout: %s
writeTimeout: %s

authMethod: http
authHTTPAddress: %s
authHTTPExclude:
  - action: api
  - action: metrics

api: true
apiAddress: %s
metrics: true
metricsAddress: %s
pprof: false
playback: false

rtsp: false
rtmp: true
rtmpEncryption: "no"
rtmpAddress: %s

hls: true
hlsAddress: %s
hlsEncryption: false
hlsAlwaysRemux: true
hlsVariant: lowLatency
hlsSegmentCount: %d
hlsSegmentDuration: %s
hlsPartDuration: %s
hlsDirectory: ""

webrtc: false
srt: false
moq: false

pathDefaults:
  source: publisher
  overridePublisher: false
  record: false

paths:
  all_others:
`, value.ReadTimeout, value.WriteTimeout, value.AuthHTTPAddress, value.APIAddress, value.MetricsAddress, value.RTMPAddress, value.HLSAddress, value.SegmentCount, value.SegmentDuration, value.PartDuration)
	return []byte(config), nil
}

type MediaMTXAuthRequestV1 struct {
	User      string `json:"user"`
	Password  string `json:"password"`
	Token     string `json:"token"`
	IP        string `json:"ip"`
	Action    string `json:"action"`
	Path      string `json:"path"`
	Protocol  string `json:"protocol"`
	ID        string `json:"id"`
	Query     string `json:"query"`
	UserAgent string `json:"userAgent"`
}

type MediaSessionLookupV1 interface {
	LoadByRunnerSessionID(string) (SessionRecordV1, *SessionSecretsV1, error)
}

type MediaMTXAuthorizerV1 struct {
	Sessions          MediaSessionLookupV1
	InternalTokenRoot string
	Now               func() time.Time
}

func (a MediaMTXAuthorizerV1) Authorize(request MediaMTXAuthRequestV1) bool {
	if a.Sessions == nil || len(a.InternalTokenRoot) < 32 || (request.Protocol != "rtmp" && request.Protocol != "hls") {
		return false
	}
	kind, runnerSessionID, _, ok := parseMediaPathV1(request.Path)
	if !ok {
		return false
	}
	record, secrets, err := a.Sessions.LoadByRunnerSessionID(runnerSessionID)
	if err != nil || record.State != "active" || record.Stopping || secrets == nil {
		return false
	}
	internalToken := InternalMediaTokenV1(a.InternalTokenRoot, runnerSessionID)
	switch {
	case kind == "ingest" && request.Protocol == "rtmp" && request.Action == "publish":
		if record.PendingKeyActivationID != "" {
			return false
		}
		current, ok := secrets.KeyIssues[secrets.CurrentKeyID]
		streamPath, streamToken, valid := ParsePrivateIngestStreamKeyV1(current.Response.StreamKey)
		expiresAt, expiryErr := time.Parse(time.RFC3339, current.Response.ExpiresAt)
		now := time.Now()
		if a.Now != nil {
			now = a.Now()
		}
		return ok && valid && expiryErr == nil && now.Before(expiresAt) && streamPath == request.Path && secureEqualV1(request.Token, streamToken)
	case kind == "ingest" && request.Protocol == "rtmp" && request.Action == "read":
		return secureEqualV1(request.Token, internalToken)
	case kind == "renditions" && request.Protocol == "rtmp" && request.Action == "publish":
		return secureEqualV1(request.Token, internalToken)
	case kind == "renditions" && request.Protocol == "hls" && request.Action == "read":
		return true
	default:
		return false
	}
}

func (a MediaMTXAuthorizerV1) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer request.Body.Close()
	request.Body = http.MaxBytesReader(writer, request.Body, 16*1024)
	var authRequest MediaMTXAuthRequestV1
	if err := DecodeStrictV1(request.Body, &authRequest); err != nil {
		http.Error(writer, "invalid authentication request", http.StatusBadRequest)
		return
	}
	if !a.Authorize(authRequest) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func IngestMediaPathV1(runnerSessionID string) (string, error) {
	if !opaqueIDPattern.MatchString(runnerSessionID) {
		return "", errors.New("invalid runner session ID")
	}
	return "ingest/" + runnerSessionID, nil
}

func RenditionMediaPathV1(runnerSessionID, rendition string) (string, error) {
	if !opaqueIDPattern.MatchString(runnerSessionID) || !opaqueIDPattern.MatchString(rendition) {
		return "", errors.New("invalid runner session or rendition ID")
	}
	return "renditions/" + runnerSessionID + "/" + rendition, nil
}

func BuildPrivateIngestStreamKeyV1(runnerSessionID, secret string) (string, error) {
	if _, err := IngestMediaPathV1(runnerSessionID); err != nil || secret == "" || len(secret) > 256 || strings.ContainsAny(secret, "\r\n") {
		return "", errors.New("invalid private ingest key")
	}
	return runnerSessionID + "?token=" + url.QueryEscape(secret), nil
}

func ParsePrivateIngestStreamKeyV1(streamKey string) (string, string, bool) {
	runnerSessionID, rawQuery, found := strings.Cut(streamKey, "?")
	if !found {
		return "", "", false
	}
	query, err := url.ParseQuery(rawQuery)
	token := query.Get("token")
	ingestPath, pathErr := IngestMediaPathV1(runnerSessionID)
	if pathErr != nil || err != nil || token == "" || len(query) != 1 || len(query["token"]) != 1 {
		return "", "", false
	}
	return ingestPath, token, true
}

func parseMediaPathV1(raw string) (kind, runnerSessionID, rendition string, ok bool) {
	if raw == "" || path.Clean(raw) != raw || strings.HasPrefix(raw, "/") {
		return "", "", "", false
	}
	parts := strings.Split(raw, "/")
	if len(parts) == 2 && parts[0] == "ingest" && opaqueIDPattern.MatchString(parts[1]) {
		return parts[0], parts[1], "", true
	}
	if len(parts) == 3 && parts[0] == "renditions" && opaqueIDPattern.MatchString(parts[1]) && opaqueIDPattern.MatchString(parts[2]) {
		return parts[0], parts[1], parts[2], true
	}
	return "", "", "", false
}

func validateListenAddressV1(address string, loopbackOnly bool) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return errors.New("listen address is invalid")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("listen port is invalid")
	}
	if !loopbackOnly {
		return nil
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("listen address must be loopback-only")
	}
	return nil
}

func secureEqualV1(left, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}

func InternalMediaTokenV1(rootSecret, runnerSessionID string) string {
	mac := hmac.New(sha256.New, []byte(rootSecret))
	_, _ = mac.Write([]byte("live-runner/media/v1:" + runnerSessionID))
	return hex.EncodeToString(mac.Sum(nil))
}
