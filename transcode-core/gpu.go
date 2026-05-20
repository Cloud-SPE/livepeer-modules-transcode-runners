package transcode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GPUVendor identifies the GPU manufacturer.
type GPUVendor string

const (
	VendorNone   GPUVendor = ""
	VendorNVIDIA GPUVendor = "nvidia"
	VendorIntel  GPUVendor = "intel"
	VendorAMD    GPUVendor = "amd"
)

// HWProfile describes the GPU hardware capabilities detected at startup.
type HWProfile struct {
	GPUName     string    `json:"gpu_name"`
	Vendor      GPUVendor `json:"vendor"`
	DevicePath  string    `json:"device_path,omitempty"`
	VRAM_MB     int       `json:"vram_mb"`
	Encoders    []string  `json:"encoders"`
	Decoders    []string  `json:"decoders"`
	HWAccels    []string  `json:"hw_accels"`
	MaxSessions int       `json:"max_sessions"`
}

type GPUDetectionReport struct {
	HWProfile
	Detected bool     `json:"detected"`
	Messages []string `json:"messages,omitempty"`
}

// DetectGPU probes the system for GPU capabilities.
// Cascading detection: NVIDIA → Intel → AMD → software-only.
func DetectGPU() HWProfile {
	report := DetectGPUReport()
	return report.HWProfile
}

// DetectGPUReport returns both the chosen hardware profile and detailed
// diagnostics for startup logging.
func DetectGPUReport() GPUDetectionReport {
	nvidia := detectNVIDIAReport()
	if nvidia.Detected {
		return nvidia
	}
	intel := detectIntelReport()
	if intel.Detected {
		return intel
	}
	amd := detectAMDReport()
	if amd.Detected {
		return amd
	}

	messages := make([]string, 0, len(nvidia.Messages)+len(intel.Messages)+len(amd.Messages)+1)
	messages = append(messages, nvidia.Messages...)
	messages = append(messages, intel.Messages...)
	messages = append(messages, amd.Messages...)
	if len(messages) == 0 {
		messages = append(messages, "no usable GPU runtime detected")
	}
	return GPUDetectionReport{
		Messages: messages,
	}
}

func detectNVIDIAReport() GPUDetectionReport {
	report := GPUDetectionReport{HWProfile: HWProfile{Vendor: VendorNVIDIA}}
	out, err := runCmd("nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits")
	if err != nil {
		report.Messages = append(report.Messages, "nvidia-smi query failed: "+err.Error())
		return report
	}

	parts := strings.SplitN(strings.TrimSpace(out), ", ", 2)
	if len(parts) == 2 {
		report.GPUName = strings.TrimSpace(parts[0])
		if vram, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil {
			report.VRAM_MB = vram
		}
	}
	report.Messages = append(report.Messages, "nvidia-smi detected "+report.GPUName)

	if out, err := runCmd("ffmpeg", "-hwaccels"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && line != "Hardware acceleration methods:" {
				report.HWAccels = append(report.HWAccels, line)
			}
		}
	} else {
		report.Messages = append(report.Messages, "ffmpeg -hwaccels failed: "+err.Error())
	}

	report.Encoders = probeEncodersByPattern("nvenc", "nv_")
	if len(report.Encoders) == 0 {
		report.Messages = append(report.Messages, "no NVIDIA encoders reported by ffmpeg")
	}

	if out, err := runCmd("ffmpeg", "-decoders"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "cuvid") || strings.Contains(line, "nv_") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					report.Decoders = append(report.Decoders, fields[1])
				}
			}
		}
	} else {
		report.Messages = append(report.Messages, "ffmpeg -decoders failed: "+err.Error())
	}

	report.MaxSessions = maxSessionsForGPU(report.GPUName, report.Vendor)
	report.HWProfile = filterNVIDIACapabilities(report.HWProfile)
	if ok, reason := nvidiaRuntimeHealthy(); !ok {
		report.Messages = append(report.Messages, "nvidia runtime sanity check failed: "+reason)
		return report
	}
	if report.GPUName == "" || len(report.Encoders) == 0 {
		report.Messages = append(report.Messages, "NVIDIA card found but no usable hardware encoders remained after filtering")
		return report
	}
	report.Detected = true
	report.Messages = append(report.Messages, "nvidia runtime sanity check passed")
	return report
}

func detectIntelReport() GPUDetectionReport {
	report := GPUDetectionReport{HWProfile: HWProfile{Vendor: VendorIntel}}
	out, err := runCmd("vainfo")
	if err != nil {
		report.Messages = append(report.Messages, "vainfo failed: "+err.Error())
		return report
	}
	if !strings.Contains(out, "iHD") && !strings.Contains(out, "i965") {
		report.Messages = append(report.Messages, "vainfo did not report an Intel VAAPI driver")
		return report
	}
	report.DevicePath = detectVAAPIDevice()
	report.GPUName = parseVAInfoGPUName(out)
	report.Messages = append(report.Messages, "vainfo detected "+report.GPUName)

	if hwaOut, err := runCmd("ffmpeg", "-hwaccels"); err == nil {
		for _, line := range strings.Split(hwaOut, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && line != "Hardware acceleration methods:" {
				report.HWAccels = append(report.HWAccels, line)
			}
		}
	} else {
		report.Messages = append(report.Messages, "ffmpeg -hwaccels failed: "+err.Error())
	}
	report.Encoders = probeEncodersByPattern("_qsv", "_vaapi")
	if decOut, err := runCmd("ffmpeg", "-decoders"); err == nil {
		for _, line := range strings.Split(decOut, "\n") {
			if strings.Contains(line, "_qsv") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					report.Decoders = append(report.Decoders, fields[1])
				}
			}
		}
	}
	report.MaxSessions = maxSessionsForGPU(report.GPUName, report.Vendor)
	report.Detected = report.GPUName != "" && len(report.Encoders) > 0
	if !report.Detected {
		report.Messages = append(report.Messages, "Intel GPU found but no usable hardware encoders detected")
	}
	return report
}

func detectAMDReport() GPUDetectionReport {
	report := GPUDetectionReport{HWProfile: HWProfile{Vendor: VendorAMD}}
	out, err := runCmd("vainfo")
	if err != nil {
		report.Messages = append(report.Messages, "vainfo failed: "+err.Error())
		return report
	}
	if !strings.Contains(out, "radeonsi") && !strings.Contains(out, "AMDGPU") {
		report.Messages = append(report.Messages, "vainfo did not report an AMD VAAPI driver")
		return report
	}
	report.DevicePath = detectVAAPIDevice()
	report.GPUName = parseVAInfoGPUName(out)
	report.Messages = append(report.Messages, "vainfo detected "+report.GPUName)

	if hwaOut, err := runCmd("ffmpeg", "-hwaccels"); err == nil {
		for _, line := range strings.Split(hwaOut, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && line != "Hardware acceleration methods:" {
				report.HWAccels = append(report.HWAccels, line)
			}
		}
	} else {
		report.Messages = append(report.Messages, "ffmpeg -hwaccels failed: "+err.Error())
	}
	report.Encoders = probeEncodersByPattern("_vaapi")
	report.MaxSessions = maxSessionsForGPU(report.GPUName, report.Vendor)
	report.Detected = report.GPUName != "" && len(report.Encoders) > 0
	if !report.Detected {
		report.Messages = append(report.Messages, "AMD GPU found but no usable hardware encoders detected")
	}
	return report
}

// Deprecated simple detectors kept for existing call sites.
func detectNVIDIA() (HWProfile, bool) {
	report := detectNVIDIAReport()
	return report.HWProfile, report.Detected
}

func detectIntel() (HWProfile, bool) {
	report := detectIntelReport()
	return report.HWProfile, report.Detected
}

func detectAMD() (HWProfile, bool) {
	report := detectAMDReport()
	return report.HWProfile, report.Detected
}

// detectVAAPIDevice finds the first DRI render node, falling back to /dev/dri/renderD128.
func detectVAAPIDevice() string {
	matches, err := filepath.Glob("/dev/dri/renderD*")
	if err == nil && len(matches) > 0 {
		return matches[0]
	}
	return "/dev/dri/renderD128"
}

// probeEncodersByPattern queries ffmpeg -encoders and returns encoders matching any pattern.
func probeEncodersByPattern(patterns ...string) []string {
	out, err := runCmd("ffmpeg", "-encoders")
	if err != nil {
		return nil
	}
	var encoders []string
	for _, line := range strings.Split(out, "\n") {
		for _, p := range patterns {
			if strings.Contains(line, p) {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					encoders = append(encoders, fields[1])
				}
				break
			}
		}
	}
	return encoders
}

// parseVAInfoGPUName extracts the GPU name from vainfo output.
// Looks for lines like "Driver version: Intel iHD driver ... - Intel(R) Xe Graphics"
func parseVAInfoGPUName(vainfo string) string {
	for _, line := range strings.Split(vainfo, "\n") {
		if strings.Contains(line, "Driver version") {
			// Try to extract a meaningful name after the last dash
			if idx := strings.LastIndex(line, " - "); idx >= 0 {
				name := strings.TrimSpace(line[idx+3:])
				if name != "" {
					return name
				}
			}
			// Fallback: return text after "Driver version:"
			marker := "Driver version:"
			if idx := strings.Index(line, marker); idx >= 0 {
				return strings.TrimSpace(line[idx+len(marker):])
			}
		}
	}
	return "Unknown GPU"
}

// HasEncoder returns true if the GPU supports the named encoder.
func (p HWProfile) HasEncoder(name string) bool {
	for _, e := range p.Encoders {
		if strings.EqualFold(e, name) {
			return true
		}
	}
	return false
}

// HasDecoder returns true if the GPU supports the named decoder.
func (p HWProfile) HasDecoder(name string) bool {
	for _, d := range p.Decoders {
		if strings.EqualFold(d, name) {
			return true
		}
	}
	return false
}

// HasHWAccel returns true if the named hardware accelerator is available.
func (p HWProfile) HasHWAccel(name string) bool {
	for _, a := range p.HWAccels {
		if strings.EqualFold(a, name) {
			return true
		}
	}
	return false
}

// IsGPUAvailable returns true if a GPU was detected with at least one encoder.
func (p HWProfile) IsGPUAvailable() bool {
	return p.GPUName != "" && len(p.Encoders) > 0
}

// maxSessionsForGPU returns the concurrent encoding session limit based on GPU vendor and model.
func maxSessionsForGPU(name string, vendor GPUVendor) int {
	switch vendor {
	case VendorIntel, VendorAMD:
		return 8
	case VendorNVIDIA:
		upper := strings.ToUpper(name)
		// Professional/datacenter GPUs — no session limit
		for _, prefix := range []string{"QUADRO", "TESLA", "A100", "A10", "A30", "A40", "L4", "L40", "H100", "H200", "B200"} {
			if strings.Contains(upper, prefix) {
				return 0 // unlimited
			}
		}
		// Consumer GPUs (GeForce, RTX, GTX, etc.)
		return 5
	default:
		return 5
	}
}

// runCmd executes a command with a 10-second timeout and returns stdout.
func runCmd(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	return string(out), err
}

func nvidiaRuntimeHealthy() (bool, string) {
	tmpDir, err := os.MkdirTemp("", "transcode-gpucheck-*")
	if err != nil {
		return false, "create temp dir: " + err.Error()
	}
	defer os.RemoveAll(tmpDir)

	inputPath := filepath.Join(tmpDir, "input.mp4")

	// First, generate a small but realistic YUV420p H.264 sample via software.
	// This mirrors go-livepeer's approach of testing a real transcode path rather
	// than relying on a synthetic encoder-only probe.
	genCtx, genCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer genCancel()
	genCmd := exec.CommandContext(genCtx, "ffmpeg",
		"-v", "error",
		"-f", "lavfi",
		"-i", "testsrc2=size=640x360:rate=30",
		"-frames:v", "30",
		"-pix_fmt", "yuv420p",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-movflags", "+faststart",
		inputPath,
	)
	if out, err := genCmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return false, "generate sample: " + msg
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-v", "error",
		"-hwaccel", "cuda",
		"-hwaccel_output_format", "cuda",
		"-c:v", "h264_cuvid",
		"-i", inputPath,
		"-frames:v", "1",
		"-c:v", "h264_nvenc",
		"-preset", "p4",
		"-tune", "hq",
		"-b:v", "2M",
		"-maxrate", "2M",
		"-bufsize", "4M",
		"-f", "null",
		"-",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return false, msg
	}
	return true, ""
}
