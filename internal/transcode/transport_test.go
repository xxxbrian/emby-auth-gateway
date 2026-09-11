package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNativeWebProfile(t *testing.T) {
	data, err := os.ReadFile("testdata/emby-web-profile.json")
	if err != nil {
		t.Fatal(err)
	}
	var profile Profile
	if err = json.Unmarshal(data, &profile); err != nil {
		t.Fatal(err)
	}
	source := testMedia()
	plan, err := Choose(source, Request{Profile: profile})
	if err != nil || plan == nil || plan.Container != "ts" || plan.AudioChannels != 2 {
		t.Fatalf("native profile plan %+v: %v", plan, err)
	}
	source.MediaStreams = append(source.MediaStreams, Stream{Index: 2, Type: "Audio", Codec: "aac", Channels: 2, SampleRate: 48000, BitRate: 192000})
	def, index := 1, 2
	source.DefaultAudioStreamIndex = &def
	plan, err = Choose(source, Request{Profile: profile, AudioStreamIndex: &index})
	if err != nil || plan == nil || plan.Audio.Index != 2 || plan.AudioCodec != "copy" {
		t.Fatalf("selected audio was not remuxed %+v: %v", plan, err)
	}
}

func TestRealTransportWindows(t *testing.T) {
	path := ffmpegFixture(t, "mkv")
	source := fileSource(t, path, "mkv", 40*TicksPerSecond)
	timeline, err := ReadTimeline(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, path) }))
	defer server.Close()
	c := &catalog{root: t.TempDir(), budget: 32 << 20, entries: map[string]*artifact{}}
	for _, start := range []int{0, 4, 1} {
		plan, err := Choose(source.Media, Request{Profile: testProfile("2")})
		if err != nil {
			t.Fatal(err)
		}
		plan.Container = "ts"
		err = runFFmpeg(context.Background(), "ffmpeg", runSpec{Input: server.URL, Plan: *plan, Timeline: timeline, Start: start, End: start + 2, Writer: func(i int) (*cacheWriter, error) { return c.begin(fmt.Sprint(i)) }, Commit: func(i int, w *cacheWriter) error { return w.commit() }})
		if err != nil {
			t.Fatalf("window %d: %v", start, err)
		}
		for i := start; i < start+2; i++ {
			f, err := c.open(fmt.Sprint(i))
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "segment.ts")
			out, err := os.Create(target)
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.Copy(out, f)
			_ = out.Close()
			_ = f.Close()
			if err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-i", target, "-map", "0:v:0", "-map", "0:a:0", "-fps_mode", "passthrough", "-enc_time_base:v", "demux", "-f", "null", "-").CombinedOutput(); err != nil || len(output) > 0 {
				t.Fatalf("segment %d decode: %v %s", i, err, output)
			}
		}
	}
}
