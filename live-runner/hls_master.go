package liverunner

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type publicHLSVariant struct {
	videoURI   string
	audioRef   string
	audioLines string
}

// MediaMTX publishes one master per rendition. Flatten its video and audio
// references into our public ladder; a variant URI must name a media playlist.
func (h *HLSHandlerV1) publicVariant(ctx context.Context, runnerID, renderPath, rendition string) (publicHLSVariant, error) {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	master, err := h.fetchPlaylist(ctx, runnerID, renderPath+"/index.m3u8")
	if err != nil {
		return publicHLSVariant{}, err
	}
	variant, audioURIs, err := flattenHLSVariant(master, rendition)
	if err != nil {
		return publicHLSVariant{}, err
	}
	for _, uri := range append(audioURIs, variant.videoURI) {
		media, err := h.fetchPlaylist(ctx, runnerID, renderPath+"/"+uri)
		if err != nil {
			return publicHLSVariant{}, err
		}
		segments, err := ParseFinalizedHLSSegmentsV1(strings.NewReader(media))
		if err != nil || len(segments) == 0 {
			return publicHLSVariant{}, errors.New("rendition media is not ready")
		}
	}
	return variant, nil
}

func flattenHLSVariant(master, rendition string) (publicHLSVariant, []string, error) {
	var result publicHLSVariant
	var audioURIs []string
	video, err := mediaPlaylistURIV1(master)
	if err != nil || !safePlaylistName(video) {
		return result, nil, errors.New("invalid video playlist")
	}
	result.videoURI = video
	var stream map[string]string
	var audio []map[string]string
	for _, line := range strings.Split(master, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") && stream == nil {
			stream, err = hlsAttributes(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
		} else if strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			var attrs map[string]string
			attrs, err = hlsAttributes(strings.TrimPrefix(line, "#EXT-X-MEDIA:"))
			if attrs["TYPE"] == "AUDIO" {
				audio = append(audio, attrs)
			}
		}
		if err != nil {
			return result, nil, err
		}
	}
	group := stream["AUDIO"]
	if group == "" {
		return result, nil, nil
	}
	newGroup := fmt.Sprintf("%q", rendition+"-audio")
	for _, attrs := range audio {
		if attrs["GROUP-ID"] != group {
			continue
		}
		uri := strings.Trim(attrs["URI"], "\"")
		if attrs["URI"] != "" {
			if !safePlaylistName(uri) {
				return result, nil, errors.New("invalid audio playlist")
			}
			audioURIs = append(audioURIs, uri)
			attrs["URI"] = fmt.Sprintf("%q", rendition+"/"+uri)
		}
		attrs["GROUP-ID"] = newGroup
		// Explicit ordering makes output stable and retains alternate audio metadata.
		var parts []string
		for _, key := range []string{"TYPE", "GROUP-ID", "NAME", "LANGUAGE", "ASSOC-LANGUAGE", "DEFAULT", "AUTOSELECT", "FORCED", "CHARACTERISTICS", "CHANNELS", "URI"} {
			if value, ok := attrs[key]; ok {
				parts = append(parts, key+"="+value)
			}
		}
		result.audioLines += "#EXT-X-MEDIA:" + strings.Join(parts, ",") + "\n"
	}
	if result.audioLines == "" {
		return result, nil, errors.New("missing audio group")
	}
	result.audioRef = ",AUDIO=" + newGroup
	return result, audioURIs, nil
}

func safePlaylistName(value string) bool {
	if !validHLSAssetNameV1(value) || !strings.HasSuffix(value, ".m3u8") {
		return false
	}
	return !strings.ContainsAny(value, "\\\"?#:%\r\n\t ")
}

// Attribute lists contain quoted commas (notably CODECS and NAME).
func hlsAttributes(raw string) (map[string]string, error) {
	out := map[string]string{}
	start, quoted := 0, false
	for i := 0; i <= len(raw); i++ {
		if i < len(raw) && raw[i] == '"' {
			quoted = !quoted
		}
		if i < len(raw) && (raw[i] != ',' || quoted) {
			continue
		}
		key, value, ok := strings.Cut(raw[start:i], "=")
		if !ok || key == "" || value == "" || out[key] != "" || quoted || strings.ContainsAny(value, "\r\n") {
			return nil, errors.New("invalid HLS attributes")
		}
		out[key] = value
		start = i + 1
	}
	return out, nil
}
