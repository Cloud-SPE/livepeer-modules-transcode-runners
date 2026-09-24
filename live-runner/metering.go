package liverunner

import (
	"bufio"
	"context"
	"errors"
	"io"
	"path"
	"strconv"
	"strings"
	"time"
)

const maxMediaPlaylistBytesV1 = 1 << 20

// FinalizedHLSSegmentV1 is a complete media segment advertised with EXTINF.
// Low-latency parts are intentionally absent: they are provisional and must
// never advance output_seconds.
type FinalizedHLSSegmentV1 struct {
	URI                  string
	DurationMicroseconds uint64
}

// ParseFinalizedHLSSegmentsV1 extracts only complete EXTINF/URI pairs from a
// media playlist. The URI is retained as the segment's stable identity across
// playlist rereads; MediaMTX gives newly finalized segments unique names.
func ParseFinalizedHLSSegmentsV1(reader io.Reader) ([]FinalizedHLSSegmentV1, error) {
	limited := io.LimitReader(reader, maxMediaPlaylistBytesV1+1)
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), maxMediaPlaylistBytesV1+1)
	lineNumber := 0
	mediaPlaylist := false
	pendingDuration := uint64(0)
	hasPendingDuration := false
	seen := make(map[string]FinalizedHLSSegmentV1)
	var segments []FinalizedHLSSegmentV1
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if lineNumber == 1 {
			line = strings.TrimPrefix(line, "\ufeff")
			if line != "#EXTM3U" {
				return nil, errors.New("HLS playlist header is invalid")
			}
			continue
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			if hasPendingDuration {
				return nil, errors.New("HLS segment duration has no URI")
			}
			raw := strings.TrimPrefix(line, "#EXTINF:")
			if comma := strings.IndexByte(raw, ','); comma >= 0 {
				raw = raw[:comma]
			}
			duration, err := parseHLSMicrosecondsV1(raw)
			if err != nil {
				return nil, err
			}
			mediaPlaylist = true
			pendingDuration, hasPendingDuration = duration, true
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if !hasPendingDuration {
			continue
		}
		if len(line) > 2048 || strings.ContainsAny(line, "\x00\r\n") {
			return nil, errors.New("HLS segment URI is invalid")
		}
		segment := FinalizedHLSSegmentV1{URI: line, DurationMicroseconds: pendingDuration}
		if previous, ok := seen[line]; ok {
			if previous.DurationMicroseconds != pendingDuration {
				return nil, errors.New("HLS segment URI has conflicting durations")
			}
		} else {
			seen[line] = segment
			segments = append(segments, segment)
		}
		hasPendingDuration = false
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.New("HLS playlist exceeds the size limit")
	}
	if lineNumber == 0 || !mediaPlaylist {
		return nil, errors.New("HLS media playlist has no finalized segments")
	}
	if hasPendingDuration {
		return nil, errors.New("HLS segment duration has no URI")
	}
	return segments, nil
}

func parseHLSMicrosecondsV1(raw string) (uint64, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.HasPrefix(raw, "-") || strings.HasPrefix(raw, "+") {
		return 0, errors.New("HLS segment duration is invalid")
	}
	parts := strings.Split(raw, ".")
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && (parts[1] == "" || len(parts[1]) > 6)) {
		return 0, errors.New("HLS segment duration is invalid")
	}
	seconds, err := strconv.ParseUint(parts[0], 10, 58)
	if err != nil {
		return 0, errors.New("HLS segment duration is invalid")
	}
	fraction := uint64(0)
	if len(parts) == 2 {
		fraction, err = strconv.ParseUint(parts[1]+strings.Repeat("0", 6-len(parts[1])), 10, 64)
		if err != nil {
			return 0, errors.New("HLS segment duration is invalid")
		}
	}
	duration := seconds*1_000_000 + fraction
	if duration == 0 {
		return 0, errors.New("HLS segment duration is invalid")
	}
	return duration, nil
}

type LiveOutputMeterV1 struct {
	store             *EncryptedFileSessionStoreV1
	hls               *HLSHandlerV1
	router            MediaPathReaderV1
	pollInterval      time.Duration
	heartbeatInterval time.Duration
	requestTimeout    time.Duration
	stallAfter        time.Duration
	failAfter         time.Duration
	now               func() time.Time
	metrics           *LiveRunnerMetricsV1
}

func NewLiveOutputMeterV1(store *EncryptedFileSessionStoreV1, hls *HLSHandlerV1, router MediaPathReaderV1, pollInterval, heartbeatInterval, requestTimeout, stallAfter, failAfter time.Duration) (*LiveOutputMeterV1, error) {
	if store == nil || hls == nil || router == nil || pollInterval <= 0 || heartbeatInterval <= 0 || requestTimeout <= 0 || pollInterval > heartbeatInterval || stallAfter <= 0 || failAfter <= stallAfter {
		return nil, errors.New("live output meter dependencies are invalid")
	}
	return &LiveOutputMeterV1{store: store, hls: hls, router: router, pollInterval: pollInterval, heartbeatInterval: heartbeatInterval, requestTimeout: requestTimeout, stallAfter: stallAfter, failAfter: failAfter, now: time.Now, metrics: &LiveRunnerMetricsV1{}}, nil
}

// Run polls the one rendition named by the immutable session parameters. A
// bad or unavailable playlist is non-billable input: the meter keeps liveness
// heartbeats flowing and retries without changing the segment cursor.
func (m *LiveOutputMeterV1) Run(ctx context.Context, record SessionRecordV1, secrets SessionSecretsV1) string {
	renderPath, err := RenditionMediaPathV1(record.RunnerSessionID, secrets.CreateRequest.SessionParams.MeteringRendition)
	if err != nil {
		return ""
	}
	ingestPath, err := IngestMediaPathV1(record.RunnerSessionID)
	if err != nil {
		return ""
	}
	if m.poll(ctx, record, ingestPath, renderPath) {
		return "output_failed"
	}
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-ticker.C:
			if m.poll(ctx, record, ingestPath, renderPath) {
				return "output_failed"
			}
		}
	}
}

func (m *LiveOutputMeterV1) poll(ctx context.Context, record SessionRecordV1, ingestPath, renderPath string) bool {
	now := m.now()
	status, statusErr := m.router.Path(ctx, ingestPath)
	ingestOnline := statusErr == nil && status.Online && status.Source != nil && (status.Source.Type == "rtmpConn" || status.Source.Type == "rtmpsConn")
	if statusErr == nil || errors.Is(statusErr, ErrMediaPathNotFoundV1) {
		if err := m.store.RecordIngestPresence(record.BrokerSessionID, ingestOnline, now); errors.Is(err, ErrSessionTerminalV1) {
			return false
		}
	}
	segments, err := m.finalizedSegments(ctx, record.RunnerSessionID, renderPath)
	if ingestOnline && err == nil {
		if _, err = m.store.RecordFinalizedSegments(record.BrokerSessionID, segments, now); errors.Is(err, ErrSessionTerminalV1) {
			return false
		}
	}
	failed := false
	if ingestOnline {
		var stalled bool
		failed, stalled, err = m.store.EvaluateOutputHealth(record.BrokerSessionID, now, m.stallAfter, m.failAfter)
		if errors.Is(err, ErrSessionTerminalV1) {
			return false
		}
		if stalled {
			m.metrics.RecordSessionStalled()
		}
	}
	_, _ = m.store.RecordHeartbeat(record.BrokerSessionID, now, m.heartbeatInterval)
	return failed
}

func (m *LiveOutputMeterV1) finalizedSegments(ctx context.Context, runnerID, renderPath string) ([]FinalizedHLSSegmentV1, error) {
	ctx, cancel := context.WithTimeout(ctx, m.requestTimeout)
	defer cancel()
	master, err := m.hls.fetchPlaylist(ctx, runnerID, renderPath+"/index.m3u8")
	if err != nil {
		return nil, err
	}
	mediaURI, err := mediaPlaylistURIV1(master)
	if err != nil {
		return nil, err
	}
	media, err := m.hls.fetchPlaylist(ctx, runnerID, renderPath+"/"+mediaURI)
	if err != nil {
		return nil, err
	}
	return ParseFinalizedHLSSegmentsV1(strings.NewReader(media))
}

func mediaPlaylistURIV1(master string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(master))
	lineNumber := 0
	wantURI := false
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if lineNumber == 1 {
			line = strings.TrimPrefix(line, "\ufeff")
			if line != "#EXTM3U" {
				return "", errors.New("HLS master playlist header is invalid")
			}
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			wantURI = true
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if wantURI && path.Base(line) == line && strings.HasSuffix(line, ".m3u8") && len(line) <= 255 {
			return line, nil
		}
		if wantURI {
			return "", errors.New("HLS media playlist URI is invalid")
		}
	}
	return "", errors.New("HLS master playlist has no media playlist")
}
