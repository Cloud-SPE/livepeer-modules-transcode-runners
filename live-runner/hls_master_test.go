package liverunner

import (
	"strings"
	"testing"
)

func TestFlattenMediaMTXAudioGroups(t *testing.T) {
	master := "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"group_audio\",NAME=\"English, stereo\",DEFAULT=YES,AUTOSELECT=YES,LANGUAGE=\"en\",CHANNELS=\"2\",URI=\"audio2_stream.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000,CODECS=\"avc1.64001f,mp4a.40.2\",AUDIO=\"group_audio\"\nvideo1_stream.m3u8\n"
	for _, rendition := range []string{"720p", "360p"} {
		got, uris, err := flattenHLSVariant(master, rendition)
		if err != nil {
			t.Fatal(err)
		}
		if got.videoURI != "video1_stream.m3u8" || len(uris) != 1 || uris[0] != "audio2_stream.m3u8" || got.audioRef != ",AUDIO=\""+rendition+"-audio\"" {
			t.Fatalf("variant=%+v uris=%v", got, uris)
		}
		for _, expected := range []string{"GROUP-ID=\"" + rendition + "-audio\"", "URI=\"" + rendition + "/audio2_stream.m3u8\"", "NAME=\"English, stereo\"", "LANGUAGE=\"en\"", "DEFAULT=YES", "CHANNELS=\"2\""} {
			if !strings.Contains(got.audioLines, expected) {
				t.Fatalf("missing %s in %s", expected, got.audioLines)
			}
		}
	}
	for _, uri := range []string{"../audio.m3u8", "https://private/audio.m3u8", "audio.m3u8?token=secret", "audio%2fsecret.m3u8", "audio\\secret.m3u8"} {
		if _, _, err := flattenHLSVariant(strings.ReplaceAll(master, "audio2_stream.m3u8", uri), "720p"); err == nil {
			t.Fatalf("accepted unsafe audio URI %q", uri)
		}
	}
	if _, _, err := flattenHLSVariant(strings.ReplaceAll(master, "AUDIO=\"group_audio\"", "AUDIO=\"missing\""), "720p"); err == nil {
		t.Fatal("accepted missing audio group")
	}
}
