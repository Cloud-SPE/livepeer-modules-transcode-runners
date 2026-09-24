package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	transcode "github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core"
)

type DurableABRExecutorV2 struct {
	store *FileWorkloadStoreV2
	hw    transcode.HWProfile
}

func NewDurableABRExecutorV2(store *FileWorkloadStoreV2, hardware transcode.HWProfile) (*DurableABRExecutorV2, error) {
	if store == nil {
		return nil, errors.New("workload store is required")
	}
	return &DurableABRExecutorV2{store: store, hw: hardware}, nil
}

func (e *DurableABRExecutorV2) Execute(ctx context.Context, req ABRWorkloadRequestV2, preset transcode.ABRPreset, reporter ABRExecutionReporterV2) (ABRTerminalResultV2, error) {
	workDir := filepath.Join(e.store.dir, "work", req.WorkloadID)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return ABRTerminalResultV2{}, executionFailureV2("storage_failed", "local workload storage failed", true)
	}
	inputPath := filepath.Join(workDir, "input"+guessExtension(req.Input.DownloadURL))
	if err := e.ensureInput(ctx, req, inputPath, reporter); err != nil {
		return ABRTerminalResultV2{}, err
	}
	if err := reporter.Progress("probing", 5, "", 0); err != nil {
		return ABRTerminalResultV2{}, err
	}
	probe, err := probeMediaV2(ctx, inputPath)
	if err != nil {
		return ABRTerminalResultV2{}, executionFailureV2("probe_failed", "input probe failed", false)
	}

	record, err := e.store.Load(req.WorkloadID)
	if err != nil {
		return ABRTerminalResultV2{}, err
	}
	delivered := make([]RenditionResultV2, 0, len(preset.Renditions))
	manifestPaths := make(map[string]string, len(preset.Renditions))
	for index, rendition := range preset.Renditions {
		if !safeJournalPathV2(rendition.Name) || filepath.Dir(rendition.Name) != "." {
			return ABRTerminalResultV2{}, executionFailureV2("preset_invalid", "selected preset is invalid", false)
		}
		manifestPaths[rendition.Name] = rendition.Name + "/playlist.m3u8"
		if checkpoint, ok := record.Delivered[rendition.Name]; ok {
			delivered = append(delivered, checkpoint)
			continue
		}

		prepared, ok := record.Prepared[rendition.Name]
		if !ok || !e.preparedFilesValid(prepared) {
			prepared, err = e.encodeRendition(ctx, req.WorkloadID, workDir, inputPath, preset, rendition, probe, index, reporter)
			if err != nil {
				return ABRTerminalResultV2{}, err
			}
			if err := reporter.Prepared(prepared); err != nil {
				return ABRTerminalResultV2{}, err
			}
		}
		if err := reporter.Progress("uploading", renditionProgressV2(index, len(preset.Renditions), 90), rendition.Name, frameCountV2(prepared.Video)); err != nil {
			return ABRTerminalResultV2{}, err
		}
		destination := req.Output.Renditions[rendition.Name]
		if err := transcode.UploadFile(ctx, filepath.Join(e.store.dir, prepared.PlaylistPath), destination.Playlist.UploadURL, nil); err != nil {
			return ABRTerminalResultV2{}, executionFailureV2("upload_failed", "rendition upload failed", true)
		}
		if err := transcode.UploadFile(ctx, filepath.Join(e.store.dir, prepared.StreamPath), destination.Stream.UploadURL, nil); err != nil {
			return ABRTerminalResultV2{}, executionFailureV2("upload_failed", "rendition upload failed", true)
		}
		result := RenditionResultV2{
			Name: rendition.Name, PlaylistURI: destination.Playlist.ArtifactURI, StreamURI: destination.Stream.ArtifactURI,
			Video: prepared.Video, FileSizeBytes: prepared.FileSizeBytes,
		}
		if err := reporter.Delivered(result); err != nil {
			return ABRTerminalResultV2{}, err
		}
		delivered = append(delivered, result)
	}

	if err := reporter.Progress("packaging", 98, "", 0); err != nil {
		return ABRTerminalResultV2{}, err
	}
	manifest := transcode.GenerateMasterPlaylist(preset.Renditions, manifestPaths)
	manifestPath := filepath.Join(workDir, "master.m3u8")
	if record.ManifestDeliveredURI == "" {
		if record.ManifestPrepared == nil || !e.preparedArtifactValid(*record.ManifestPrepared) {
			if err := writeFileAtomicV2(manifestPath, []byte(manifest), 0o600); err != nil {
				return ABRTerminalResultV2{}, executionFailureV2("storage_failed", "manifest packaging failed", true)
			}
			manifestHash, err := fileSHA256V2(manifestPath)
			if err != nil {
				return ABRTerminalResultV2{}, executionFailureV2("storage_failed", "manifest packaging failed", true)
			}
			relative, _ := filepath.Rel(e.store.dir, manifestPath)
			if err := reporter.PreparedManifest(PreparedArtifactV2{Path: relative, SHA256: manifestHash}); err != nil {
				return ABRTerminalResultV2{}, err
			}
		} else {
			manifestPath = filepath.Join(e.store.dir, record.ManifestPrepared.Path)
		}
		if err := transcode.UploadFile(ctx, manifestPath, req.Output.Manifest.UploadURL, nil); err != nil {
			return ABRTerminalResultV2{}, executionFailureV2("upload_failed", "manifest upload failed", true)
		}
		if err := reporter.DeliveredManifest(req.Output.Manifest.ArtifactURI); err != nil {
			return ABRTerminalResultV2{}, err
		}
	}
	units, err := CalculateFrameMegapixelUnitsV2(delivered)
	if err != nil {
		return ABRTerminalResultV2{}, executionFailureV2("usage_failed", "usage measurement failed", false)
	}
	hash, err := RequestContentSHA256V2(req)
	if err != nil {
		return ABRTerminalResultV2{}, err
	}
	return ABRTerminalResultV2{
		Schema: ABRResultSchemaV2, WorkloadID: req.WorkloadID, RequestSHA256: hash, Outcome: "succeeded",
		ManifestURI: req.Output.Manifest.ArtifactURI, Renditions: delivered,
		Usage: UsageClaimV2{Unit: ABRWorkUnitV2, Units: units},
	}, nil
}

func (e *DurableABRExecutorV2) ensureInput(ctx context.Context, req ABRWorkloadRequestV2, inputPath string, reporter ABRExecutionReporterV2) error {
	if _, err := os.Stat(inputPath); err == nil {
		if req.Input.ContentSHA256 == "" {
			return nil
		}
		hash, err := fileSHA256V2(inputPath)
		if err == nil && hash == req.Input.ContentSHA256 {
			return nil
		}
	}
	if err := reporter.Progress("downloading", 0, "", 0); err != nil {
		return err
	}
	partial := inputPath + ".partial"
	_ = os.Remove(partial)
	if _, err := transcode.DownloadFile(ctx, req.Input.DownloadURL, partial, nil); err != nil {
		_ = os.Remove(partial)
		return executionFailureV2("download_failed", "input download failed", true)
	}
	if req.Input.ContentSHA256 != "" {
		hash, err := fileSHA256V2(partial)
		if err != nil || hash != req.Input.ContentSHA256 {
			_ = os.Remove(partial)
			return executionFailureV2("input_hash_mismatch", "input content hash did not match", false)
		}
	}
	if err := os.Rename(partial, inputPath); err != nil {
		return executionFailureV2("storage_failed", "local workload storage failed", true)
	}
	return nil
}

func (e *DurableABRExecutorV2) encodeRendition(ctx context.Context, workloadID, workDir, inputPath string, preset transcode.ABRPreset, rendition transcode.ABRRendition, probe transcode.ProbeResult, index int, reporter ABRExecutionReporterV2) (PreparedRenditionV2, error) {
	renditionDir := filepath.Join(workDir, rendition.Name)
	if err := os.MkdirAll(renditionDir, 0o700); err != nil {
		return PreparedRenditionV2{}, executionFailureV2("storage_failed", "local workload storage failed", true)
	}
	_ = os.Remove(filepath.Join(renditionDir, "playlist.m3u8"))
	_ = os.Remove(filepath.Join(renditionDir, "stream.mp4"))
	if err := reporter.Progress("encoding", renditionProgressV2(index, len(preset.Renditions), 5), rendition.Name, 0); err != nil {
		return PreparedRenditionV2{}, err
	}
	cmd := transcode.HLSRenditionCmdContext(ctx, inputPath, renditionDir, rendition, preset.SegmentDuration, e.hw, probe)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return PreparedRenditionV2{}, executionFailureV2("encode_failed", "rendition encoding failed", true)
	}
	if err := cmd.Start(); err != nil {
		return PreparedRenditionV2{}, executionFailureV2("encode_failed", "rendition encoding failed", true)
	}
	scanner := bufio.NewScanner(stderr)
	scanner.Split(scanFFmpegLines)
	lastReport := time.Time{}
	var progressErr error
	for scanner.Scan() {
		info, ok := transcode.ParseProgressLine(scanner.Text())
		if !ok || time.Since(lastReport) < time.Second {
			continue
		}
		lastReport = time.Now()
		percent := transcode.CalcPercent(info.Time, time.Duration(probe.Duration*float64(time.Second)))
		if err := reporter.Progress("encoding", renditionProgressV2(index, len(preset.Renditions), percent), rendition.Name, uint64(max(info.Frame, 0))); err != nil {
			progressErr = err
			_ = cmd.Process.Kill()
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		if progressErr != nil {
			return PreparedRenditionV2{}, progressErr
		}
		if ctx.Err() != nil {
			return PreparedRenditionV2{}, ctx.Err()
		}
		return PreparedRenditionV2{}, executionFailureV2("encode_failed", "rendition encoding failed", true)
	}
	if err := scanner.Err(); err != nil {
		return PreparedRenditionV2{}, executionFailureV2("encode_failed", "rendition progress failed", true)
	}
	playlistPath := filepath.Join(renditionDir, "playlist.m3u8")
	streamPath := filepath.Join(renditionDir, "stream.mp4")
	video, err := exactDeliveredVideoV2(ctx, streamPath, rendition.Video != nil)
	if err != nil {
		return PreparedRenditionV2{}, executionFailureV2("usage_failed", "output frame measurement failed", false)
	}
	playlistHash, err := fileSHA256V2(playlistPath)
	if err != nil {
		return PreparedRenditionV2{}, executionFailureV2("storage_failed", "encoded output validation failed", true)
	}
	streamHash, err := fileSHA256V2(streamPath)
	if err != nil {
		return PreparedRenditionV2{}, executionFailureV2("storage_failed", "encoded output validation failed", true)
	}
	stat, err := os.Stat(streamPath)
	if err != nil || stat.Size() < 0 {
		return PreparedRenditionV2{}, executionFailureV2("storage_failed", "encoded output validation failed", true)
	}
	playlistRelative, _ := filepath.Rel(e.store.dir, playlistPath)
	streamRelative, _ := filepath.Rel(e.store.dir, streamPath)
	return PreparedRenditionV2{
		Name: rendition.Name, PlaylistPath: playlistRelative, StreamPath: streamRelative,
		PlaylistSHA256: playlistHash, StreamSHA256: streamHash, Video: video, FileSizeBytes: uint64(stat.Size()),
	}, nil
}

func (e *DurableABRExecutorV2) preparedFilesValid(prepared PreparedRenditionV2) bool {
	playlistHash, err := fileSHA256V2(filepath.Join(e.store.dir, prepared.PlaylistPath))
	if err != nil || playlistHash != prepared.PlaylistSHA256 {
		return false
	}
	streamHash, err := fileSHA256V2(filepath.Join(e.store.dir, prepared.StreamPath))
	return err == nil && streamHash == prepared.StreamSHA256
}

func (e *DurableABRExecutorV2) preparedArtifactValid(prepared PreparedArtifactV2) bool {
	hash, err := fileSHA256V2(filepath.Join(e.store.dir, prepared.Path))
	return err == nil && hash == prepared.SHA256
}

type frameProbeOutputV2 struct {
	Streams []struct {
		Frames string `json:"nb_read_frames"`
		Width  uint32 `json:"width"`
		Height uint32 `json:"height"`
	} `json:"streams"`
}

func exactDeliveredVideoV2(ctx context.Context, path string, hasVideo bool) (*DeliveredVideoV2, error) {
	if !hasVideo {
		return nil, nil
	}
	output, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-count_frames", "-select_streams", "v:0", "-show_entries", "stream=nb_read_frames,width,height", "-of", "json", path).Output()
	if err != nil {
		return nil, err
	}
	var probe frameProbeOutputV2
	if err := json.Unmarshal(output, &probe); err != nil || len(probe.Streams) != 1 {
		return nil, errors.New("missing video frame count")
	}
	frames, err := strconv.ParseUint(probe.Streams[0].Frames, 10, 64)
	if err != nil || frames == 0 || probe.Streams[0].Width == 0 || probe.Streams[0].Height == 0 {
		return nil, errors.New("invalid video frame count")
	}
	return &DeliveredVideoV2{ActualFrames: frames, Width: probe.Streams[0].Width, Height: probe.Streams[0].Height}, nil
}

func probeMediaV2(ctx context.Context, path string) (transcode.ProbeResult, error) {
	output, err := exec.CommandContext(ctx, "ffprobe", "-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", path).Output()
	if err != nil {
		return transcode.ProbeResult{}, err
	}
	return transcode.ParseProbeOutput(output)
}

func fileSHA256V2(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeFileAtomicV2(path string, body []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".artifact-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func executionFailureV2(code, message string, retryable bool) error {
	return &ABRExecutionErrorV2{Code: code, Message: message, Retryable: retryable}
}

func renditionProgressV2(index, count int, within float64) float64 {
	if count <= 0 {
		return 5
	}
	return 5 + ((float64(index)+within/100)/float64(count))*90
}

func frameCountV2(video *DeliveredVideoV2) uint64 {
	if video == nil {
		return 0
	}
	return video.ActualFrames
}
