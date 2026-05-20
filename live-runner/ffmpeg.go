package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

func buildRuntimePlan(cfg config, rec sessionRecord, hw transcode.HWProfile) (buildRuntime, error) {
	if err := os.MkdirAll(rec.OutputDir, 0o755); err != nil {
		return buildRuntime{}, err
	}
	for idx := range rec.Preset.Renditions {
		if err := os.MkdirAll(filepath.Join(rec.OutputDir, variantDirName(idx, rec.Preset.Renditions[idx].Name)), 0o755); err != nil {
			return buildRuntime{}, err
		}
	}

	listenURL := fmt.Sprintf("rtmp://%s:%d/live/%s?listen=1", cfg.RTMPListenHost, rec.RTMPPort, rec.StreamKey)
	args := buildLiveFFmpegArgs(listenURL, rec.OutputDir, rec.Preset, hw, cfg.HLSWindowSegments)
	return buildRuntime{
		Args:      args,
		ListenURL: listenURL,
		OutputDir: rec.OutputDir,
		MasterURL: rec.HLSURL,
		UsageUnit: "output_seconds",
		CreatedAt: time.Now().UTC(),
	}, nil
}

func startFFmpeg(rt *sessionRuntime, plan buildRuntime, hw transcode.HWProfile) error {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, rt.cfg.FFmpegBin, plan.Args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return err
	}
	cmd.Stdout = nil
	if err := cmd.Start(); err != nil {
		cancel()
		return err
	}
	rt.ffmpegCancel = cancel
	rt.ffmpegDone = make(chan struct{})

	go func() {
		defer close(rt.ffmpegDone)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if p, ok := transcode.ParseProgressLine(line); ok {
				total := uint64(p.Time / time.Second)
				delta, started := rt.markProgress(total)
				if started {
					rt.event("session.started", total, 0, "", map[string]any{"listen_url": plan.ListenURL})
				}
				if delta > 0 {
					rt.event("session.usage.tick", total, delta, "", nil)
				}
			}
		}
		err := cmd.Wait()
		if rt.terminating.Load() {
			return
		}
		if err != nil {
			log.Printf("[live %s] ffmpeg exited: %v", rt.rec.RunnerSessionID, err)
			rt.fail("ffmpeg_exit_nonzero")
			rt.event("session.failed", rt.lastUsageTotal.Load(), 0, "ffmpeg_exit_nonzero", map[string]any{
				"error_text": err.Error(),
				"gpu":        hw.GPUName,
			})
			return
		}
		rt.finishTerminate("completed", false)
		rt.event("session.ended", rt.lastUsageTotal.Load(), 0, "completed", nil)
	}()
	return nil
}

func buildLiveFFmpegArgs(listenURL, outputDir string, preset transcode.ABRPreset, hw transcode.HWProfile, window int) []string {
	args := []string{
		"-y",
		"-fflags", "+nobuffer",
		"-flags", "+low_delay",
		"-i", listenURL,
	}

	videoCount := 0
	filterParts := make([]string, 0, len(preset.Renditions)+1)
	for idx, rung := range preset.Renditions {
		if rung.Video == nil {
			continue
		}
		_ = idx
		filterParts = append(filterParts, fmt.Sprintf("[0:v]split=%d%s", len(videoRenditions(preset)), joinLabels(len(videoRenditions(preset)))))
		videoCount++
		break
	}
	if videoCount > 0 {
		filterParts = filterGraphForPreset(preset)
		args = append(args, "-filter_complex", strings.Join(filterParts, ";"))
	}

	streamMap := make([]string, 0, len(preset.Renditions))
	outIndex := 0
	for idx, rung := range preset.Renditions {
		vlabel := fmt.Sprintf("[v%dout]", idx)
		if rung.Video != nil {
			args = append(args, "-map", vlabel)
			args = append(args, "-map", "0:a?")
			args = append(args, perOutputArgs(outIndex, rung, hw)...)
			streamMap = append(streamMap, fmt.Sprintf("v:%d,a:%d,name:%s", outIndex, outIndex, sanitizeName(rung.Name)))
			outIndex++
			continue
		}
		args = append(args, "-map", "0:a?")
		args = append(args,
			fmt.Sprintf("-c:a:%d", outIndex), rung.Audio.Codec,
			fmt.Sprintf("-b:a:%d", outIndex), rung.Audio.Bitrate,
		)
		streamMap = append(streamMap, fmt.Sprintf("a:%d,name:%s", outIndex, sanitizeName(rung.Name)))
		outIndex++
	}

	args = append(args,
		"-f", "hls",
		"-hls_time", itoa(max(2, preset.SegmentDuration)),
		"-hls_list_size", itoa(max(3, window)),
		"-hls_flags", "delete_segments+append_list+independent_segments+omit_endlist",
		"-hls_segment_filename", filepath.Join(outputDir, "v%v", "segment_%06d.ts"),
		"-master_pl_name", "master.m3u8",
		"-var_stream_map", strings.Join(streamMap, " "),
		filepath.Join(outputDir, "v%v", "playlist.m3u8"),
	)
	return args
}

func filterGraphForPreset(preset transcode.ABRPreset) []string {
	vrs := videoRenditions(preset)
	if len(vrs) == 0 {
		return nil
	}
	parts := []string{fmt.Sprintf("[0:v]split=%d%s", len(vrs), joinLabels(len(vrs)))}
	for idx, rung := range vrs {
		if rung.Video == nil {
			continue
		}
		parts = append(parts,
			fmt.Sprintf("[vin%d]scale=w=%d:h=%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2[v%dout]",
				idx, rung.Video.Width, rung.Video.Height, rung.Video.Width, rung.Video.Height, idx))
	}
	return parts
}

func joinLabels(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(fmt.Sprintf("[vin%d]", i))
	}
	return b.String()
}

func videoRenditions(preset transcode.ABRPreset) []transcode.ABRRendition {
	return preset.VideoRenditions()
}

func perOutputArgs(idx int, rung transcode.ABRRendition, hw transcode.HWProfile) []string {
	encoder := transcode.EncoderForCodec(rung.Video.Codec, hw)
	args := []string{
		fmt.Sprintf("-c:v:%d", idx), encoder,
		fmt.Sprintf("-b:v:%d", idx), rung.Video.Bitrate,
		fmt.Sprintf("-maxrate:v:%d", idx), fallbackString(rung.Video.MaxBitrate, rung.Video.Bitrate),
	}
	if rung.Video.BufSize != "" {
		args = append(args, fmt.Sprintf("-bufsize:v:%d", idx), rung.Video.BufSize)
	}
	if rung.Video.Profile != "" {
		args = append(args, fmt.Sprintf("-profile:v:%d", idx), rung.Video.Profile)
	}
	if rung.Video.Level != "" {
		args = append(args, fmt.Sprintf("-level:v:%d", idx), rung.Video.Level)
	}
	args = append(args,
		fmt.Sprintf("-c:a:%d", idx), fallbackString(rung.Audio.Codec, "aac"),
		fmt.Sprintf("-b:a:%d", idx), fallbackString(rung.Audio.Bitrate, "128k"),
	)
	if rung.Audio.Channels > 0 {
		args = append(args, fmt.Sprintf("-ac:%d", idx), itoa(rung.Audio.Channels))
	}
	if rung.Audio.SampleRate > 0 {
		args = append(args, fmt.Sprintf("-ar:%d", idx), itoa(rung.Audio.SampleRate))
	}
	return args
}

func sanitizeName(s string) string {
	if s == "" {
		return "default"
	}
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "/", "_")
	return s
}

func variantDirName(idx int, _ string) string { return fmt.Sprintf("v%d", idx) }

func fallbackString(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
