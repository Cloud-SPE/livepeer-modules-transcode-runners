package transcode

import (
	"fmt"
	"strings"
)

type NVIDIAGeneration string

const (
	NVIDIAGenerationUnknown NVIDIAGeneration = "unknown"
	NVIDIAGenerationPascal  NVIDIAGeneration = "pascal"
	NVIDIAGenerationTuring  NVIDIAGeneration = "turing"
	NVIDIAGenerationAmpere  NVIDIAGeneration = "ampere"
	NVIDIAGenerationAda     NVIDIAGeneration = "ada"
	NVIDIAGenerationBlackwell NVIDIAGeneration = "blackwell"
)

type NVIDIACapabilityProfile struct {
	Generation  NVIDIAGeneration
	SupportsAV1 bool
}

func DetectNVIDIACapabilityProfile(name string) NVIDIACapabilityProfile {
	upper := strings.ToUpper(name)
	switch {
	case containsAny(upper, "GTX 10", "P100", "P40", "P4", "TITAN XP", "TITAN X"):
		return NVIDIACapabilityProfile{Generation: NVIDIAGenerationPascal}
	case containsAny(upper, "RTX 20", "T4", "QUADRO RTX", "TITAN RTX"):
		return NVIDIACapabilityProfile{Generation: NVIDIAGenerationTuring}
	case containsAny(upper, "RTX 30", "A10", "A16", "A30", "A40", "A100", "RTX A", "A2", "A2000", "A4000", "A4500", "A5000", "A5500", "A6000"):
		return NVIDIACapabilityProfile{Generation: NVIDIAGenerationAmpere}
	case containsAny(upper, "RTX 40", "RTX 4000 ADA", "RTX 4500 ADA", "RTX 5000 ADA", "RTX 5880 ADA", "RTX 6000 ADA", "L4", "L40", "L40S"):
		return NVIDIACapabilityProfile{Generation: NVIDIAGenerationAda, SupportsAV1: true}
	case containsAny(upper, "RTX 50", "B200", "GB200"):
		return NVIDIACapabilityProfile{Generation: NVIDIAGenerationBlackwell, SupportsAV1: true}
	default:
		return NVIDIACapabilityProfile{Generation: NVIDIAGenerationUnknown}
	}
}

func containsAny(s string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// StrictGPUIncompatibleFeatures returns request features that currently force
// CPU-side processing and are therefore rejected in strict hardware mode.
func StrictGPUIncompatibleFeatures(opts TranscodeOptions, wantsThumbnail bool) []string {
	var features []string
	if opts.SubtitlePath != "" {
		features = append(features, "subtitle burn-in")
	}
	if opts.WatermarkPath != "" {
		features = append(features, "watermark overlay")
	}
	if wantsThumbnail {
		features = append(features, "thumbnail extraction")
	}
	return features
}

func CanHardwareEncodeCodec(hw HWProfile, codec string) bool {
	return EncoderForCodec(codec, hw) != softwareEncoderForCodec(codec)
}

func CanHardwareDecodeCodec(hw HWProfile, codec string) bool {
	switch hw.Vendor {
	case VendorNVIDIA, VendorIntel:
		return DecoderForCodec(codec, hw) != ""
	case VendorAMD:
		if !hw.HasHWAccel("vaapi") {
			return false
		}
		codec = strings.ToLower(codec)
		profile := DetectAMDCapabilityProfile(hw.GPUName)
		for _, supported := range profile.DecodeCodecs {
			if supported == codec {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// ValidateStrictGPUInput fails closed when the current host cannot sustain
// a full hardware path for the given input codec.
func ValidateStrictGPUInput(hw HWProfile, inputCodec string) error {
	if !hw.IsGPUAvailable() {
		return fmt.Errorf("strict GPU mode: no GPU available")
	}
	switch hw.Vendor {
	case VendorNVIDIA:
		if !hw.HasHWAccel("cuda") {
			return fmt.Errorf("strict GPU mode: CUDA hwaccel unavailable")
		}
	case VendorIntel:
		if !hw.HasHWAccel("qsv") {
			return fmt.Errorf("strict GPU mode: QSV hwaccel unavailable")
		}
	case VendorAMD:
		if !hw.HasHWAccel("vaapi") {
			return fmt.Errorf("strict GPU mode: VAAPI hwaccel unavailable")
		}
	default:
		return fmt.Errorf("strict GPU mode: unsupported GPU vendor")
	}
	if inputCodec == "" {
		return fmt.Errorf("strict GPU mode: unknown input codec")
	}
	if !CanHardwareDecodeCodec(hw, inputCodec) {
		return fmt.Errorf("strict GPU mode: no hardware decoder available for input codec %q", inputCodec)
	}
	return nil
}

// ValidateStrictGPUEncode fails closed when the chosen output codec would not
// use a hardware encoder on the current GPU.
func ValidateStrictGPUEncode(hw HWProfile, outputCodec string) error {
	if !CanHardwareEncodeCodec(hw, outputCodec) {
		return fmt.Errorf("strict GPU mode: no hardware encoder available for output codec %q", outputCodec)
	}
	return nil
}

func filterNVIDIACapabilities(hw HWProfile) HWProfile {
	profile := DetectNVIDIACapabilityProfile(hw.GPUName)
	if !profile.SupportsAV1 {
		hw.Encoders = filterOutValue(hw.Encoders, "av1_nvenc")
		hw.Decoders = filterOutValue(hw.Decoders, "av1_cuvid")
	}
	return hw
}

func filterOutValue(values []string, target string) []string {
	out := values[:0]
	for _, v := range values {
		if !strings.EqualFold(v, target) {
			out = append(out, v)
		}
	}
	return out
}

type AMDCapabilityProfile struct {
	DecodeCodecs []string
}

func DetectAMDCapabilityProfile(name string) AMDCapabilityProfile {
	upper := strings.ToUpper(name)
	switch {
	case containsAny(upper, "7900", "7800", "7700", "NAVI31", "NAVI32", "NAVI33"):
		return AMDCapabilityProfile{DecodeCodecs: []string{"h264", "hevc", "vp9", "av1"}}
	case containsAny(upper, "6900", "6800", "6700", "6600", "NAVI21", "NAVI22", "NAVI23", "NAVI24"):
		return AMDCapabilityProfile{DecodeCodecs: []string{"h264", "hevc", "vp9"}}
	default:
		return AMDCapabilityProfile{DecodeCodecs: []string{"h264", "hevc"}}
	}
}

type IntelCapabilityProfile struct {
	DecodeCodecs []string
}

func DetectIntelCapabilityProfile(name string) IntelCapabilityProfile {
	upper := strings.ToUpper(name)
	switch {
	case containsAny(upper, "ARC", "A770", "A750", "A580", "A380"):
		return IntelCapabilityProfile{DecodeCodecs: []string{"h264", "hevc", "vp9", "av1"}}
	default:
		return IntelCapabilityProfile{DecodeCodecs: []string{"h264", "hevc", "vp9"}}
	}
}
