package liverunner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

type HLSHandlerV1 struct {
	store   *EncryptedFileSessionStoreV1
	presets map[string]transcode.ABRPreset
	baseURL string
	client  *http.Client
	timeout time.Duration
	mu      sync.RWMutex
	cookies map[string][]*http.Cookie
}

func NewHLSHandlerV1(store *EncryptedFileSessionStoreV1, presets []transcode.ABRPreset, upstream string, transport http.RoundTripper, timeout time.Duration) (*HLSHandlerV1, error) {
	parsed, err := url.Parse(upstream)
	host, _, splitErr := net.SplitHostPort(parsed.Host)
	ip := net.ParseIP(host)
	if store == nil || len(presets) == 0 || err != nil || splitErr != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || (host != "localhost" && (ip == nil || !ip.IsLoopback())) || timeout <= 0 {
		return nil, errors.New("HLS proxy dependencies are invalid")
	}
	byName := make(map[string]transcode.ABRPreset, len(presets))
	for _, preset := range presets {
		if preset.Name == "" || len(preset.Renditions) == 0 {
			return nil, errors.New("HLS proxy preset is invalid")
		}
		byName[strings.ToLower(preset.Name)] = preset
	}
	if transport == nil {
		base := http.DefaultTransport.(*http.Transport).Clone()
		base.ResponseHeaderTimeout = timeout
		base.DisableCompression = true
		transport = base
	}
	return &HLSHandlerV1{store: store, presets: byName, baseURL: strings.TrimRight(upstream, "/"), timeout: timeout, cookies: make(map[string][]*http.Cookie), client: &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (h *HLSHandlerV1) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		writeRunnerErrorV1(writer, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	record, secrets, err := h.store.LoadByRunnerSessionID(request.PathValue("id"))
	if err != nil || secrets == nil || record.State != "active" || record.Stopping {
		writeRunnerErrorV1(writer, http.StatusNotFound, "stream_not_found")
		return
	}
	preset, ok := h.presets[strings.ToLower(secrets.CreateRequest.SessionParams.OutputProfile)]
	if !ok {
		writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "profile_unavailable")
		return
	}
	asset := request.PathValue("asset")
	if rendition := request.PathValue("rendition"); rendition != "" {
		asset = rendition + "/" + asset
	} else if asset == "" && strings.HasSuffix(request.URL.Path, "/master.m3u8") {
		asset = "master.m3u8"
	}
	if asset == "master.m3u8" {
		h.serveMaster(writer, request, record.RunnerSessionID, preset)
		return
	}
	renderPath, ok := renditionAssetPathV1(record.RunnerSessionID, preset, asset)
	if !ok || !validHLSQueryV1(request.URL.Query()) {
		writeRunnerErrorV1(writer, http.StatusNotFound, "asset_not_found")
		return
	}
	h.proxyAsset(writer, request, record.RunnerSessionID, renderPath)
}

func (h *HLSHandlerV1) serveMaster(writer http.ResponseWriter, request *http.Request, runnerID string, preset transcode.ABRPreset) {
	var body strings.Builder
	body.WriteString("#EXTM3U\n#EXT-X-VERSION:9\n")
	playable := 0
	for _, rendition := range preset.Renditions {
		if rendition.Video == nil {
			continue
		}
		renderPath, err := RenditionMediaPathV1(runnerID, rendition.Name)
		if err != nil || !h.hasFinalizedSegment(request.Context(), runnerID, renderPath) {
			continue
		}
		videoBandwidth, videoErr := bitrateBitsV1(rendition.Video.MaxBitrate)
		audioBandwidth, audioErr := bitrateBitsV1(rendition.Audio.Bitrate)
		codec, codecErr := h264CodecV1(rendition.Video.Profile, rendition.Video.Level)
		if videoErr != nil || audioErr != nil || codecErr != nil {
			writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "profile_unavailable")
			return
		}
		fmt.Fprintf(&body, "#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,CODECS=\"%s,mp4a.40.2\"\n%s/index.m3u8\n", videoBandwidth+audioBandwidth, rendition.Video.Width, rendition.Video.Height, codec, rendition.Name)
		playable++
	}
	if playable == 0 {
		writer.Header().Set("Retry-After", "1")
		writeRunnerErrorV1(writer, http.StatusServiceUnavailable, "output_unavailable")
		return
	}
	writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	writer.Header().Set("Cache-Control", "no-store")
	if request.Method == http.MethodGet {
		_, _ = io.WriteString(writer, body.String())
	}
}

func (h *HLSHandlerV1) hasFinalizedSegment(ctx context.Context, runnerID, renderPath string) bool {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	master, err := h.fetchPlaylist(ctx, runnerID, renderPath+"/index.m3u8")
	if err != nil {
		return false
	}
	mediaURI, err := mediaPlaylistURIV1(master)
	if err != nil {
		return false
	}
	media, err := h.fetchPlaylist(ctx, runnerID, renderPath+"/"+mediaURI)
	if err != nil {
		return false
	}
	segments, err := ParseFinalizedHLSSegmentsV1(strings.NewReader(media))
	return err == nil && len(segments) > 0
}

func (h *HLSHandlerV1) fetchPlaylist(ctx context.Context, runnerID, mediaPath string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, h.baseURL+"/"+mediaPath, nil)
	if err != nil {
		return "", errors.New("HLS playlist request failed")
	}
	h.addSessionCookies(runnerID, request)
	response, err := h.client.Do(request)
	if err != nil {
		return "", errors.New("HLS playlist request failed")
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		response, err = h.followMediaMTXCookieCheck(ctx, runnerID, request, response)
		if err != nil {
			return "", err
		}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return "", errors.New("HLS playlist is unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxMediaPlaylistBytesV1+1))
	if err != nil || len(body) > maxMediaPlaylistBytesV1 {
		return "", errors.New("HLS playlist is invalid")
	}
	return string(body), nil
}

func (h *HLSHandlerV1) proxyAsset(writer http.ResponseWriter, request *http.Request, runnerID, mediaPath string) {
	upstream, err := http.NewRequestWithContext(request.Context(), request.Method, h.baseURL+"/"+mediaPath, nil)
	if err != nil {
		writeRunnerErrorV1(writer, http.StatusBadGateway, "media_unavailable")
		return
	}
	upstream.URL.RawQuery = request.URL.RawQuery
	if value := request.Header.Get("Range"); value != "" {
		upstream.Header.Set("Range", value)
	}
	h.addSessionCookies(runnerID, upstream)
	response, err := h.client.Do(upstream)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		writeRunnerErrorV1(writer, http.StatusBadGateway, "media_unavailable")
		return
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		response, err = h.followMediaMTXCookieCheck(request.Context(), runnerID, upstream, response)
		if err != nil {
			writeRunnerErrorV1(writer, http.StatusBadGateway, "media_unavailable")
			return
		}
	}
	defer response.Body.Close()
	for _, name := range []string{"Content-Type", "Content-Length", "Accept-Ranges", "Content-Range"} {
		if value := response.Header.Get(name); value != "" {
			writer.Header().Set(name, value)
		}
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(response.StatusCode)
	if request.Method == http.MethodGet {
		_, _ = io.Copy(writer, response.Body)
	}
}

func (h *HLSHandlerV1) followMediaMTXCookieCheck(ctx context.Context, runnerID string, original *http.Request, redirect *http.Response) (*http.Response, error) {
	defer redirect.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(redirect.Body, 4096))
	location, err := redirect.Location()
	if err != nil || location.Scheme != original.URL.Scheme || location.Host != original.URL.Host || location.Path != original.URL.Path {
		return nil, errors.New("untrusted HLS redirect")
	}
	query := location.Query()
	if query.Get("cookieCheck") != "1" || len(query["cookieCheck"]) != 1 {
		return nil, errors.New("untrusted HLS redirect")
	}
	query.Del("cookieCheck")
	if query.Encode() != original.URL.Query().Encode() || len(redirect.Cookies()) == 0 {
		return nil, errors.New("untrusted HLS redirect")
	}
	follow, err := http.NewRequestWithContext(ctx, original.Method, location.String(), nil)
	if err != nil {
		return nil, errors.New("HLS cookie check request failed")
	}
	follow.Header.Set("Range", original.Header.Get("Range"))
	for _, cookie := range redirect.Cookies() {
		follow.AddCookie(cookie)
	}
	h.storeSessionCookies(runnerID, redirect.Cookies())
	response, err := h.client.Do(follow)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		response.Body.Close()
		return nil, errors.New("repeated HLS redirect")
	}
	h.storeSessionCookies(runnerID, response.Cookies())
	return response, nil
}

func (h *HLSHandlerV1) addSessionCookies(runnerID string, request *http.Request) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, cookie := range h.cookies[runnerID] {
		copy := *cookie
		request.AddCookie(&copy)
	}
}

func (h *HLSHandlerV1) storeSessionCookies(runnerID string, cookies []*http.Cookie) {
	if len(cookies) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	byName := make(map[string]*http.Cookie, len(h.cookies[runnerID])+len(cookies))
	for _, cookie := range h.cookies[runnerID] {
		copy := *cookie
		byName[cookie.Name] = &copy
	}
	for _, cookie := range cookies {
		copy := *cookie
		byName[cookie.Name] = &copy
	}
	merged := make([]*http.Cookie, 0, len(byName))
	for _, cookie := range byName {
		merged = append(merged, cookie)
	}
	h.cookies[runnerID] = merged
}

func renditionAssetPathV1(runnerID string, preset transcode.ABRPreset, asset string) (string, bool) {
	if asset == "" || path.Clean(asset) != asset || strings.HasPrefix(asset, "/") {
		return "", false
	}
	parts := strings.Split(asset, "/")
	if len(parts) != 2 || !opaqueIDPattern.MatchString(parts[0]) || !validHLSAssetNameV1(parts[1]) {
		return "", false
	}
	declared := false
	for _, rendition := range preset.Renditions {
		declared = declared || rendition.Name == parts[0]
	}
	mediaPath, err := RenditionMediaPathV1(runnerID, parts[0])
	return mediaPath + "/" + parts[1], declared && err == nil
}

func validHLSAssetNameV1(name string) bool {
	if name == "" || path.Base(name) != name || strings.HasPrefix(name, ".") {
		return false
	}
	for _, suffix := range []string{".m3u8", ".mp4", ".m4s", ".ts"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func validHLSQueryV1(values url.Values) bool {
	for key, entries := range values {
		if key != "_HLS_msn" && key != "_HLS_part" && key != "_HLS_skip" {
			return false
		}
		if len(entries) != 1 || entries[0] == "" || strings.ContainsAny(entries[0], "\r\n") {
			return false
		}
		if key == "_HLS_skip" {
			if entries[0] != "YES" && entries[0] != "v2" {
				return false
			}
		} else if _, err := strconv.ParseUint(entries[0], 10, 64); err != nil {
			return false
		}
	}
	return true
}

func h264CodecV1(profile, level string) (string, error) {
	if profile == "" {
		profile = "high"
	}
	if level == "" {
		level = "3.1"
	}
	profileID, ok := map[string]string{"baseline": "42", "main": "4d", "high": "64"}[strings.ToLower(profile)]
	parts := strings.Split(level, ".")
	if !ok || len(parts) != 2 {
		return "", errors.New("invalid H264 profile or level")
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	levelID := major*10 + minor
	if majorErr != nil || minorErr != nil || levelID < 0 || levelID > 255 {
		return "", errors.New("invalid H264 profile or level")
	}
	return fmt.Sprintf("avc1.%s00%02x", profileID, levelID), nil
}

func bitrateBitsV1(raw string) (uint64, error) {
	multiplier := float64(1)
	value := raw
	if strings.HasSuffix(raw, "M") {
		multiplier, value = 1_000_000, strings.TrimSuffix(raw, "M")
	} else if strings.HasSuffix(raw, "k") || strings.HasSuffix(raw, "K") {
		multiplier, value = 1_000, raw[:len(raw)-1]
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed <= 0 {
		return 0, errors.New("invalid bitrate")
	}
	return uint64(parsed * multiplier), nil
}
