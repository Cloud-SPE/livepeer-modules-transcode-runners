package transcode

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// LiveRTMPOutput is one encoded rendition published back into the local media
// router. URL can contain a short-lived router credential and must not be
// logged or persisted by callers.
type LiveRTMPOutput struct {
	Rendition        ABRRendition
	URL              string
	KeyframeInterval time.Duration
}

// LiveLadderCmdContext builds one FFmpeg process that decodes a live RTMP
// source once and publishes every rendition as RTMP. The media router turns
// those rendition streams into LL-HLS; FFmpeg does not pretend its ordinary
// HLS muxer implements partial-segment LL-HLS.
func LiveLadderCmdContext(ctx context.Context, inputURL string, outputs []LiveRTMPOutput, hw HWProfile, probe ProbeResult) (*exec.Cmd, error) {
	if strings.TrimSpace(inputURL) == "" || len(outputs) == 0 {
		return nil, errors.New("live input and at least one output are required")
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostats", "-progress", "pipe:2",
		"-fflags", "+nobuffer", "-flags", "+low_delay",
	}
	args = append(args, buildHWAccelInputArgs(hw)...)
	args = append(args, "-i", inputURL)

	seen := make(map[string]struct{}, len(outputs))
	for _, output := range outputs {
		rendition := output.Rendition
		if strings.TrimSpace(rendition.Name) == "" || strings.TrimSpace(output.URL) == "" {
			return nil, errors.New("live output identity and URL are required")
		}
		if _, duplicate := seen[rendition.Name]; duplicate {
			return nil, errors.New("live output rendition names must be unique")
		}
		seen[rendition.Name] = struct{}{}

		if rendition.Video == nil {
			args = append(args, "-map", "0:a:0?", "-vn")
		} else {
			if output.KeyframeInterval <= 0 || output.KeyframeInterval > 10*time.Second {
				return nil, errors.New("live keyframe interval must be between zero and ten seconds")
			}
			if !strings.EqualFold(rendition.Video.Codec, "h264") && !strings.EqualFold(rendition.Video.Codec, "avc") {
				return nil, errors.New("RTMP live outputs require H.264 video")
			}
			args = append(args, "-map", "0:v:0", "-map", "0:a:0?")
			args = append(args, buildHLSVideoArgs(rendition, hw)...)
			seconds := strconv.FormatFloat(output.KeyframeInterval.Seconds(), 'f', 3, 64)
			args = append(args, "-force_key_frames", "expr:gte(t,n_forced*"+seconds+")")
			if filters := buildLiveFilterGraph(rendition, hw, probe); filters != "" {
				args = append(args, "-vf", filters)
			}
		}
		args = append(args, buildHLSAudioArgs(rendition)...)
		args = append(args, "-f", "flv", output.URL)
	}
	return exec.CommandContext(ctx, "ffmpeg", args...), nil
}

func buildLiveFilterGraph(rendition ABRRendition, hw HWProfile, probe ProbeResult) string {
	video := rendition.Video
	if video == nil || video.Width <= 0 || video.Height <= 0 || (video.Width == probe.Width && video.Height == probe.Height) {
		return ""
	}
	// LiveLadderCmdContext requests hardware-output decoding when an accelerator
	// is available, so frames are already resident on that device. Adding an
	// upload filter here would upload an already-hardware frame and fail.
	return buildScaleFilter(video.Width, video.Height, hw)
}
