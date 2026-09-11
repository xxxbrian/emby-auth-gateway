package transcode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testMedia() MediaSource {
	return MediaSource{ID: "source", Container: "mkv", Size: 100_000_000, Bitrate: 4_000_000, RunTimeTicks: 40 * TicksPerSecond, MediaStreams: []Stream{{Index: 0, Type: "Video", Codec: "h264", Profile: "High", Width: 1920, Height: 1080, BitDepth: 8, Level: 41}, {Index: 1, Type: "Audio", Codec: "eac3", Channels: 6, BitRate: 640000, SampleRate: 48000}}}
}
func testProfile(channels string) Profile {
	return Profile{DirectPlayProfiles: []FormatProfile{{Type: "Video", Container: "mkv,mp4", VideoCodec: "h264,hevc", AudioCodec: "aac"}}, TranscodingProfiles: []FormatProfile{{Type: "Video", Container: "m4s,ts", Protocol: "hls", VideoCodec: "h264,hevc", AudioCodec: "aac", MaxAudioChannels: channels}}}
}
func TestChooseAudioCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name             string
		source           func(*MediaSource)
		request          func(*Request)
		channels         int
		direct, rejected bool
	}{
		{name: "stereo", channels: 2},
		{name: "surround", request: func(r *Request) { r.Profile = testProfile("6") }, channels: 6},
		{name: "explicit stereo", request: func(r *Request) { r.Profile = testProfile("6"); r.MaxAudioChannels = 2 }, channels: 2},
		{name: "compatible original", source: func(s *MediaSource) { s.MediaStreams[1].Codec = "aac" }, direct: true},
		{name: "unsupported video", source: func(s *MediaSource) { s.MediaStreams[0].Codec = "mpeg2video" }, rejected: true},
		{name: "video profile constraint", request: func(r *Request) {
			r.Profile.CodecProfiles = []CodecProfile{{Type: "Video", Codec: "h264", Conditions: []Condition{{Property: "Width", Condition: "LessThanEqual", Value: "1280", IsRequired: true}}}}
		}, rejected: true},
		{name: "bandwidth keeps audio-only copy", request: func(r *Request) { r.MaxStreamingBitrate = 1_000_000 }, channels: 2},
		{name: "client refuses aac", request: func(r *Request) { r.Profile.TranscodingProfiles[0].AudioCodec = "opus" }, rejected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testMedia()
			r := Request{Profile: testProfile("2")}
			if tc.source != nil {
				tc.source(&s)
			}
			if tc.request != nil {
				tc.request(&r)
			}
			plan, err := Choose(s, r)
			if tc.rejected {
				if !errors.Is(err, ErrUnsupported) {
					t.Fatalf("got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.direct {
				if plan != nil {
					t.Fatal("compatible original was converted")
				}
				return
			}
			if plan == nil || plan.AudioCodec != "aac" || plan.AudioChannels != tc.channels {
				t.Fatalf("unexpected plan %+v", plan)
			}
		})
	}
}

func TestChooseCopiesHEVCWhenHLSProfileListsServerEncoders(t *testing.T) {
	source := testMedia()
	source.MediaStreams[0].Codec = "hevc"
	source.Bitrate = 12_573_135
	profile := testProfile("2")
	profile.DirectPlayProfiles[0].VideoCodec = "h264,av1"
	profile.TranscodingProfiles[0].VideoCodec = "h264,av1"

	plan, err := Choose(source, Request{Profile: profile, MaxStreamingBitrate: 7_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil || plan.AudioCodec != "aac" || plan.AudioChannels != 2 || !strings.EqualFold(plan.Video.Codec, "hevc") {
		t.Fatalf("expected audio-only HEVC copy plan, got %+v", plan)
	}
}

type sectionCloser struct {
	io.Reader
	io.Closer
}

func fileSource(t *testing.T, path string, container string, duration int64) Source {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	media := testMedia()
	media.Container, media.Size, media.RunTimeTicks = container, st.Size(), duration
	return Source{Media: media, Open: func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return sectionCloser{io.NewSectionReader(f, offset, length), f}, nil
	}}
}
func ffmpegFixture(t *testing.T, container string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		if os.Getenv("GATEWAY_REQUIRE_FFMPEG_TESTS") == "1" {
			t.Fatal(err)
		}
		t.Skip("ffmpeg is unavailable")
	}
	path := filepath.Join(t.TempDir(), "fixture."+container)
	args := []string{"-v", "error", "-nostdin", "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "40", "-c:v", "libx264", "-preset", "ultrafast", "-threads:v", "1", "-g", "48", "-keyint_min", "48", "-sc_threshold", "0", "-c:a", "eac3", "-ac", "6", "-b:a", "384k", path}
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, out)
	}
	return path
}
func TestIndexedContainers(t *testing.T) {
	for _, container := range []string{"mkv", "mp4"} {
		t.Run(container, func(t *testing.T) {
			path := ffmpegFixture(t, container)
			source := fileSource(t, path, container, 40*TicksPerSecond)
			timeline, err := ReadTimeline(context.Background(), source)
			if err != nil {
				t.Fatal(err)
			}
			if timeline.Len() < 8 || timeline.Len() > 11 {
				t.Fatalf("cuts %v", timeline.Cuts)
			}
			for i := 1; i < timeline.Len(); i++ {
				if timeline.Cuts[i] <= timeline.Cuts[i-1] {
					t.Fatal("non increasing index")
				}
			}
		})
	}
}
func TestRealFFmpegWindows(t *testing.T) {
	path := ffmpegFixture(t, "mkv")
	source := fileSource(t, path, "mkv", 40*TicksPerSecond)
	timeline, err := ReadTimeline(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, path) }))
	defer server.Close()
	cache := &catalog{root: t.TempDir(), budget: 32 << 20, entries: map[string]*artifact{}}
	var firstTracks []movieTrack
	var initData []byte
	for _, start := range []int{0, 4, 1} {
		plan, err := Choose(source.Media, Request{Profile: testProfile("2")})
		if err != nil {
			t.Fatal(err)
		}
		err = runFFmpeg(context.Background(), "ffmpeg", runSpec{Input: server.URL, Plan: *plan, Timeline: timeline, Start: start, End: start + 2,
			Init: func(b []byte, tracks []movieTrack) error {
				if firstTracks == nil {
					firstTracks = tracks
					initData = append([]byte(nil), b...)
				} else if !sameTracks(firstTracks, tracks) {
					return fmt.Errorf("initialization changed")
				}
				return nil
			},
			Writer: func(index int) (*cacheWriter, error) { return cache.begin(fmt.Sprint(index)) },
			Commit: func(index int, w *cacheWriter) error { return w.commit() },
		})
		if err != nil {
			t.Fatalf("window %d: %v", start, err)
		}
		joined := filepath.Join(t.TempDir(), "window.mp4")
		out, e := os.Create(joined)
		if e != nil {
			t.Fatal(e)
		}
		_, _ = out.Write(initData)
		for i := start; i < start+2; i++ {
			f, e := cache.open(fmt.Sprint(i))
			if e != nil {
				t.Fatalf("segment %d: %v", i, e)
			}
			_, e = io.Copy(out, f)
			_ = f.Close()
			if e != nil {
				t.Fatal(e)
			}
		}
		_ = out.Close()
		cmd := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-i", joined, "-map", "0:v:0", "-map", "0:a:0", "-fps_mode", "passthrough", "-enc_time_base:v", "demux", "-f", "null", "-")
		if output, e := cmd.CombinedOutput(); e != nil || len(output) != 0 {
			t.Fatalf("decode window %d: %v %s", start, e, output)
		}
	}
}
func TestCachePinsAndReservation(t *testing.T) {
	c := &catalog{root: t.TempDir(), budget: 8, entries: map[string]*artifact{}}
	if err := c.store("a", []byte("12345678")); err != nil {
		t.Fatal(err)
	}
	a, err := c.open("a")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.store("b", []byte("x")); !errors.Is(err, ErrCapacity) {
		t.Fatalf("pinned file evicted: %v", err)
	}
	_ = a.Close()
	if err = c.store("b", []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if c.has("a") {
		t.Fatal("old artifact retained")
	}
	w, err := c.begin("c")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(w, strings.NewReader("123456789")); !errors.Is(err, ErrCapacity) {
		t.Fatalf("unbounded write: %v", err)
	}
	w.abort()
	used, reserved, _, _, _, _ := c.stats()
	if used+reserved > c.budget || reserved != 0 {
		t.Fatalf("accounting used=%d reserved=%d", used, reserved)
	}
}

func TestCacheProtectsPendingDemand(t *testing.T) {
	c := &catalog{root: t.TempDir(), budget: 8, entries: map[string]*artifact{}}
	c.protect("wanted", 1)
	if err := c.store("wanted", []byte("12345678")); err != nil {
		t.Fatal(err)
	}
	if err := c.store("prefetch", []byte("x")); !errors.Is(err, ErrCapacity) {
		t.Fatalf("requested output was evicted: %v", err)
	}
	reader, err := c.open("wanted")
	if err != nil {
		t.Fatal(err)
	}
	c.protect("wanted", -1)
	if err = c.store("prefetch", []byte("x")); !errors.Is(err, ErrCapacity) {
		t.Fatal("reader lost its pin")
	}
	_ = reader.Close()
	if err = c.store("prefetch", []byte("x")); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedSubtitlesKeepTheirDeliveryContract(t *testing.T) {
	source := testMedia()
	index := 2
	source.DefaultSubtitleStreamIndex = &index
	source.MediaStreams = append(source.MediaStreams, Stream{Index: 2, Type: "Subtitle", Codec: "subrip", DeliveryMethod: "External", DeliveryURL: "/Videos/item/source/Subtitles/2/0/Stream.vtt"})
	if plan, err := Choose(source, Request{Profile: testProfile("2")}); err != nil || plan == nil {
		t.Fatalf("external subtitle prevented audio conversion: %v", err)
	}
	source.MediaStreams[2].DeliveryMethod = "Encode"
	if _, err := Choose(source, Request{Profile: testProfile("2")}); !errors.Is(err, ErrUnsupported) {
		t.Fatal("subtitle burn-in was silently omitted")
	}
	off := -1
	if plan, err := Choose(source, Request{Profile: testProfile("2"), SubtitleStreamIndex: &off}); err != nil || plan == nil {
		t.Fatal("disabled subtitle prevented conversion")
	}
}
