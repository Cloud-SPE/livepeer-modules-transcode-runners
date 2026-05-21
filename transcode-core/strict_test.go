package transcode

import "testing"

func TestValidateVideoProbe(t *testing.T) {
	t.Run("video present", func(t *testing.T) {
		if err := ValidateVideoProbe(ProbeResult{VideoCodec: "h264", AudioCodec: "aac"}); err != nil {
			t.Fatalf("ValidateVideoProbe() error = %v, want nil", err)
		}
	})

	t.Run("audio only", func(t *testing.T) {
		err := ValidateVideoProbe(ProbeResult{AudioCodec: "mp3"})
		if err == nil {
			t.Fatal("ValidateVideoProbe() error = nil, want error")
		}
		if got, want := err.Error(), `input has no video stream (detected audio codec "mp3")`; got != want {
			t.Fatalf("ValidateVideoProbe() error = %q, want %q", got, want)
		}
	})

	t.Run("no media streams", func(t *testing.T) {
		err := ValidateVideoProbe(ProbeResult{})
		if err == nil {
			t.Fatal("ValidateVideoProbe() error = nil, want error")
		}
		if got, want := err.Error(), "input has no video stream"; got != want {
			t.Fatalf("ValidateVideoProbe() error = %q, want %q", got, want)
		}
	})
}
