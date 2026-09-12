// A deterministic Emby-compatible upstream for native Web subtitle recovery tests.
// All media is synthesized locally; it never contacts an upstream account.
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
)

func main() {
	dir := flag.String("dir", "", "temporary media directory (required)")
	address := flag.String("http", "127.0.0.1:18096", "listen address")
	input := flag.String("file", "", "optional existing MKV sample")
	durationSeconds := flag.Int("duration", 40, "duration of generated test media")
	flag.Parse()
	if *dir == "" {
		log.Fatal("--dir is required")
	}
	if err := os.MkdirAll(*dir, 0700); err != nil {
		log.Fatal(err)
	}
	file := *input
	if file == "" {
		file = filepath.Join(*dir, "fixture.mkv")
		if _, err := os.Stat(file); os.IsNotExist(err) {
			for name, text := range map[string]string{"chinese": "第一句中文字幕", "english": "First English subtitle"} {
				content := "1\n00:00:01,000 --> 00:00:10,000\n" + text + "\n\n2\n00:00:20,000 --> 00:00:35,000\n" + map[string]string{"chinese": "后面的中文字幕", "english": "Later English subtitle"}[name] + "\n"
				if err := os.WriteFile(filepath.Join(*dir, name+".srt"), []byte(content), 0600); err != nil {
					log.Fatal(err)
				}
			}
			args := []string{
				"-hide_banner", "-nostdin", "-v", "error",
				"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24000/1001",
				"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
				"-f", "srt", "-i", filepath.Join(*dir, "chinese.srt"), "-f", "srt", "-i", filepath.Join(*dir, "english.srt"),
				"-t", strconv.Itoa(*durationSeconds), "-map", "0:v:0", "-map", "1:a:0", "-map", "1:a:0", "-map", "2:0", "-map", "3:0",
				"-c:v", "libx264", "-preset", "ultrafast", "-threads:v", "1", "-g", "48", "-keyint_min", "48", "-sc_threshold", "0",
				"-c:a:0", "eac3", "-ac:a:0", "6", "-b:a:0", "384k",
				"-c:a:1", "aac", "-ac:a:1", "2", "-b:a:1", "192k", "-c:s", "srt",
				"-metadata:s:s:0", "language=chi", "-metadata:s:s:1", "language=eng", file,
			}
			if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				log.Fatalf("fixture generation: %v %s", err, out)
			}
		}
	}
	probe, err := exec.Command("ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json", file).Output()
	if err != nil {
		log.Fatal(err)
	}
	var metadata struct {
		Format struct {
			Duration string `json:"duration"`
			Size     string `json:"size"`
			BitRate  string `json:"bit_rate"`
		} `json:"format"`
		Streams []map[string]any `json:"streams"`
	}
	if err = json.Unmarshal(probe, &metadata); err != nil {
		log.Fatal(err)
	}
	duration, _ := strconv.ParseFloat(metadata.Format.Duration, 64)
	size, _ := strconv.ParseInt(metadata.Format.Size, 10, 64)
	bitrate, _ := strconv.ParseInt(metadata.Format.BitRate, 10, 64)
	streams := []map[string]any{}
	defaultAudio := -1
	for _, stream := range metadata.Streams {
		kind, _ := stream["codec_type"].(string)
		if kind != "video" && kind != "audio" && kind != "subtitle" {
			continue
		}
		codec, _ := stream["codec_name"].(string)
		index := int(stream["index"].(float64))
		profile, _ := stream["profile"].(string)
		s := map[string]any{"Index": index, "Type": strings.ToUpper(kind[:1]) + kind[1:], "Codec": codec, "Profile": profile, "IsExternal": false, "IsDefault": false, "DisplayTitle": strings.ToUpper(codec), "Language": "eng"}
		for key, target := range map[string]string{"width": "Width", "height": "Height", "level": "Level", "channels": "Channels", "channel_layout": "ChannelLayout", "refs": "RefFrames"} {
			if v, ok := stream[key]; ok {
				s[target] = v
			}
		}
		for key, target := range map[string]string{"bit_rate": "BitRate", "sample_rate": "SampleRate", "bits_per_raw_sample": "BitDepth"} {
			if raw, ok := stream[key].(string); ok {
				if n, e := strconv.ParseInt(raw, 10, 64); e == nil {
					s[target] = n
				}
			}
		}
		if raw, ok := stream["avg_frame_rate"].(string); ok {
			a, b, ok := strings.Cut(raw, "/")
			if ok {
				num, _ := strconv.ParseFloat(a, 64)
				den, _ := strconv.ParseFloat(b, 64)
				if den != 0 {
					s["RealFrameRate"] = num / den
					s["AverageFrameRate"] = num / den
				}
			}
		}
		if kind == "audio" {
			if defaultAudio < 0 {
				defaultAudio = index
				s["IsDefault"] = true
			}
			s["DisplayTitle"] = fmt.Sprintf("English %s %v ch", strings.ToUpper(codec), stream["channels"])
		}
		if kind == "subtitle" {
			s["IsTextSubtitleStream"], s["DeliveryMethod"] = true, "External"
			s["DeliveryUrl"] = fmt.Sprintf("/Videos/fixture/fixture-source/Subtitles/%d/0/Stream.vtt", index)
			if index == 3 {
				s["Language"], s["DisplayTitle"], s["IsDefault"] = "chi", "Chinese Simplified (SUBRIP)", true
			} else {
				s["Language"], s["DisplayTitle"] = "eng", "English (SUBRIP)"
			}
		}
		streams = append(streams, s)
	}
	streams = append(streams, map[string]any{"Index": 5, "Type": "Subtitle", "Codec": "vtt", "Language": "eng", "IsExternal": true, "IsTextSubtitleStream": true, "DeliveryMethod": "External", "DeliveryUrl": "/Videos/fixture/fixture-source/Subtitles/5/0/Stream.vtt", "DisplayTitle": "English external (VTT)"})
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		log.Fatal(err)
	}
	base := "http://" + listener.Addr().String() + "/emby"
	if err = os.WriteFile(filepath.Join(*dir, "upstream-url"), []byte(base), 0600); err != nil {
		log.Fatal(err)
	}
	media := map[string]any{"Id": "fixture-source", "Name": "Subtitle compatibility sample", "Container": "mkv", "Protocol": "File", "Path": "/fixture/source.mkv", "Size": size, "Bitrate": bitrate, "RunTimeTicks": int64(duration * 1e7), "SupportsDirectPlay": true, "SupportsDirectStream": true, "SupportsTranscoding": false, "DefaultAudioStreamIndex": defaultAudio, "DefaultSubtitleStreamIndex": 3, "HasSubtitles": true, "DirectStreamUrl": base + "/Videos/fixture/original.mkv?MediaSourceId=fixture-source&Static=true&api_key=fixture-token", "MediaStreams": streams}
	item := map[string]any{"Id": "fixture", "ServerId": "fixture-server", "Name": "Subtitle compatibility fixture", "Type": "Movie", "MediaType": "Video", "IsFolder": false, "RunTimeTicks": int64(duration * 1e7), "MediaSources": []any{media}, "MediaStreams": streams, "MediaSourceCount": 1, "Container": "mkv", "Overview": "Locally generated subtitle recovery test. Two embedded text tracks and one external subtitle.", "ProductionYear": 2026, "LocationType": "FileSystem", "UserData": map[string]any{"Played": false, "PlaybackPositionTicks": 0, "IsFavorite": false, "PlayCount": 0}, "People": []any{}, "Genres": []string{"Test"}, "Studios": []any{}, "Chapters": []any{map[string]any{"StartPositionTicks": 0, "Name": "Start"}, map[string]any{"StartPositionTicks": 60 * 1e7, "Name": "One minute"}}}
	user := map[string]any{"Id": "fixture-user", "Name": "fixture", "Policy": map[string]any{"EnableMediaPlayback": true, "EnableAudioPlaybackTranscoding": false, "EnableVideoPlaybackTranscoding": false, "EnablePlaybackRemuxing": false}, "Configuration": map[string]any{}}
	media["DateCreated"], item["DateCreated"] = "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	var rawRequests, rawBytes, subtitleRequests atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/emby")
		log.Printf("%s %s", r.Method, path)
		switch {
		case path == "/eag-fixture/stats":
			write(w, map[string]int64{"rawRequests": rawRequests.Load(), "rawBytes": rawBytes.Load(), "subtitleRequests": subtitleRequests.Load()})
		case strings.Contains(path, "/Subtitles/"):
			subtitleRequests.Add(1)
			w.Header().Set("Content-Type", "text/vtt")
			if strings.Contains(path, "/Subtitles/5/") {
				_, _ = io.WriteString(w, "WEBVTT\n\n00:00:01.000 --> 00:00:10.000\nExternal first cue\n\n00:00:20.000 --> 00:00:35.000\nExternal later cue\n")
			}
			return
		case path == "/embywebsocket" && strings.EqualFold(r.Header.Get("Upgrade"), "websocket"):
			conn, buffer, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			digest := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
			_, _ = fmt.Fprintf(buffer, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(digest[:]))
			_ = buffer.Flush()
			_, _ = io.Copy(io.Discard, buffer)
			return
		case strings.HasPrefix(path, "/usersettings/"):
			write(w, map[string]any{})
		case path == "/Playback/BitrateTest":
			n, _ := strconv.Atoi(r.URL.Query().Get("Size"))
			if n <= 0 {
				n = 1 << 20
			}
			n = min(n, 4<<20)
			w.Header().Set("Content-Length", strconv.Itoa(n))
			_, _ = w.Write(make([]byte, n))
		case strings.HasSuffix(path, "/Ancestors") || strings.HasSuffix(path, "/LocalTrailers") || strings.HasSuffix(path, "/SpecialFeatures"):
			write(w, []any{})
		case strings.HasSuffix(path, "/ThemeMedia"):
			write(w, map[string]any{"ThemeVideosResult": map[string]any{"Items": []any{}}, "ThemeSongsResult": map[string]any{"Items": []any{}}})
		case strings.HasSuffix(path, "/Intros") || strings.HasSuffix(path, "/Similar"):
			write(w, map[string]any{"Items": []any{}, "TotalRecordCount": 0})
		case path == "/System/Info/Public" || path == "/System/Info":
			write(w, map[string]any{"Id": "fixture-server", "ServerName": "Local subtitle test", "Version": "4.9.5.0", "LocalAddress": base, "WanAddress": base})
		case path == "/Users/AuthenticateByName":
			write(w, map[string]any{"AccessToken": "fixture-token", "ServerId": "fixture-server", "User": user})
		case path == "/System/Endpoint":
			write(w, map[string]any{"IsInNetwork": true, "IsLocal": false})
		case path == "/Items/fixture/PlaybackInfo":
			var body map[string]json.RawMessage
			_ = json.NewDecoder(ioLimit(r.Body)).Decode(&body)
			if profile := body["DeviceProfile"]; len(profile) > 0 {
				_ = os.WriteFile(filepath.Join(*dir, "device-profile.json"), profile, 0600)
			}
			write(w, map[string]any{"MediaSources": []any{media}, "PlaySessionId": "fixture-upstream-play"})
		case path == "/Videos/fixture/original.mkv":
			rawRequests.Add(1)
			w.Header().Set("ETag", `"synthetic-subtitle-fixture-v1"`)
			http.ServeFile(&countedWriter{ResponseWriter: w, count: &rawBytes}, r, file)
		case strings.HasSuffix(path, "/Items/fixture") || path == "/Items/fixture":
			write(w, item)
		case strings.Contains(path, "/Images/"):
			http.NotFound(w, r)
		case strings.HasSuffix(path, "/Views"):
			write(w, map[string]any{"Items": []any{map[string]any{"Id": "fixture-library", "Name": "Movies", "Type": "CollectionFolder", "CollectionType": "movies", "IsFolder": true}}, "TotalRecordCount": 1})
		case strings.Contains(path, "/Items") || strings.HasSuffix(path, "/Latest"):
			if strings.Contains(path, "/Latest") {
				write(w, []any{item})
			} else {
				write(w, map[string]any{"Items": []any{item}, "TotalRecordCount": 1})
			}
		case strings.HasPrefix(path, "/Users/"):
			write(w, user)
		case strings.HasPrefix(path, "/Sessions") || strings.Contains(path, "ActiveEncodings"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(path, "/DisplayPreferences"):
			write(w, map[string]any{"CustomPrefs": map[string]string{}})
		default:
			write(w, []any{})
		}
	})
	log.Printf("fixture ready at %s", base)
	log.Fatal(http.Serve(listener, handler))
}

// Profile capture is bounded and contains no login credentials.
func ioLimit(r io.Reader) io.Reader { return io.LimitReader(r, 2<<20) }

type countedWriter struct {
	http.ResponseWriter
	count *atomic.Int64
}

func (w *countedWriter) Write(b []byte) (int, error) {
	n, e := w.ResponseWriter.Write(b)
	w.count.Add(int64(n))
	return n, e
}
