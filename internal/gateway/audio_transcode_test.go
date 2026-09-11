package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
	"github.com/xxxbrian/emby-auth-gateway/internal/transcode"
)

func TestAudioPlaybackOptionsUseQueryAndBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/emby/Items/1/PlaybackInfo?IsPlayback=true&StartTimeTicks=123000000&AudioStreamIndex=2&MaxAudioChannels=6", strings.NewReader(`{"DeviceProfile":{"TranscodingProfiles":[{"Type":"Video","Protocol":"hls","AudioCodec":"aac","Container":"m4s"}]}}`))
	parsed, err := withAudioNegotiation(r)
	if err != nil {
		t.Fatal(err)
	}
	n := parsed.Context().Value(audioNegotiationKey{}).(*audioNegotiation)
	if !n.playback || n.start != 123000000 || n.request.AudioStreamIndex == nil || *n.request.AudioStreamIndex != 2 || n.request.MaxAudioChannels != 6 {
		t.Fatalf("options: %+v", n)
	}
	data, _ := io.ReadAll(parsed.Body)
	if !bytes.Contains(data, []byte("DeviceProfile")) {
		t.Fatal("proxy request body was consumed")
	}
	r = httptest.NewRequest(http.MethodPost, "/emby/Items/1/PlaybackInfo?AudioStreamIndex=1", strings.NewReader(`{"DeviceProfile":{},"AudioStreamIndex":2}`))
	if _, err = withAudioNegotiation(r); err == nil {
		t.Fatal("conflicting options accepted")
	}
}

func TestGatewayAudioPlaybackLifecycle(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		if os.Getenv("GATEWAY_REQUIRE_FFMPEG_TESTS") == "1" {
			t.Fatal(err)
		}
		t.Skip("ffmpeg unavailable")
	}
	mediaFile := filepath.Join(t.TempDir(), "source.mkv")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "20", "-c:v", "libx264", "-preset", "ultrafast", "-threads:v", "1", "-g", "48", "-sc_threshold", "0", "-c:a", "eac3", "-ac", "6", mediaFile).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	st, _ := os.Stat(mediaFile)
	media := map[string]any{"Id": "source-1", "Name": "HD source", "Container": "mkv", "Size": st.Size(), "Bitrate": 1000000, "RunTimeTicks": 200000000, "SupportsDirectPlay": true, "SupportsDirectStream": true, "SupportsTranscoding": false, "DefaultAudioStreamIndex": 1, "DirectStreamUrl": "/Videos/item-1/original.mkv?MediaSourceId=source-1&api_key=backend-token", "MediaStreams": []any{map[string]any{"Index": 0, "Type": "Video", "Codec": "h264", "Width": 64, "Height": 64, "Profile": "Constrained Baseline", "Level": 10}, map[string]any{"Index": 1, "Type": "Audio", "Codec": "eac3", "Channels": 6, "BitRate": 448000, "SampleRate": 48000}}}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/PlaybackInfo"):
			writeTestJSON(w, map[string]any{"PlaySessionId": "upstream-play", "MediaSources": []any{media}})
		case strings.HasSuffix(r.URL.Path, "/original.mkv"):
			if r.Header.Get("X-Emby-Token") != "backend-token" || r.URL.Query().Get("api_key") != "backend-token" {
				http.Error(w, "auth", 401)
				return
			}
			http.ServeFile(w, r, mediaFile)
		case strings.HasSuffix(r.URL.Path, "/Items/item-1"):
			writeTestJSON(w, map[string]any{"Id": "item-1", "Type": "Movie", "Name": "Fixture movie", "MediaType": "Video", "RunTimeTicks": 200000000, "MediaSources": []any{media}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	manager, err := transcode.New(context.Background(), transcode.Config{CacheDir: t.TempDir(), Workers: 2, CacheBytes: 32 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	gateway := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", Transcoder: manager, Meter: telemetry.NewByteMeter()}, store))
	defer gateway.Close()
	profile := `{"DeviceProfile":{"DirectPlayProfiles":[{"Type":"Video","Container":"mkv,mp4","VideoCodec":"h264","AudioCodec":"aac"}],"TranscodingProfiles":[{"Type":"Video","Container":"m4s,ts","Protocol":"hls","VideoCodec":"h264","AudioCodec":"aac","MaxAudioChannels":"2"}]}}`
	req := mustRequest(t, http.MethodPost, gateway.URL+"/emby/Items/item-1/PlaybackInfo?IsPlayback=true&MediaSourceId=source-1&api_key=gateway-token", strings.NewReader(profile))
	req.Header.Set("Content-Type", "application/json")
	response := do(t, req)
	var result map[string]any
	if err = json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	id, _ := result["PlaySessionId"].(string)
	if !strings.HasPrefix(id, "eag-") {
		t.Fatalf("local playback not negotiated: %+v", result)
	}
	source := result["MediaSources"].([]any)[0].(map[string]any)
	if source["SupportsDirectStream"] != false || source["SupportsTranscoding"] != true {
		t.Fatalf("flags: %+v", source)
	}
	playlistURL := gateway.URL + "/emby" + source["TranscodingUrl"].(string)
	response = do(t, mustRequest(t, http.MethodGet, playlistURL, nil))
	playlist, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(playlist), "#EXT-X-ENDLIST") {
		t.Fatalf("playlist %d %s", response.StatusCode, playlist)
	}
	base := gateway.URL + "/emby/Videos/item-1/audio/" + id + "/"
	for _, file := range []string{"init.mp4", "3.m4s", "0.m4s"} {
		response = do(t, mustRequest(t, http.MethodGet, base+file+"?api_key=gateway-token", nil))
		data, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != 200 || len(data) < 32 {
			t.Fatalf("%s: %d %s", file, response.StatusCode, data)
		}
	}
	if len(store.PlaybackStates) != 0 {
		t.Fatal("preparation mutated playback state")
	}
	response = do(t, mustRequest(t, http.MethodGet, strings.Replace(base, "item-1", "another-item", 1)+"0.m4s?api_key=gateway-token", nil))
	_ = response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatal("item binding was bypassed")
	}
	response = do(t, mustRequest(t, http.MethodPost, gateway.URL+"/emby/Videos/ActiveEncodings/Delete?PlaySessionId="+id+"&api_key=gateway-token", nil))
	_ = response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatal("local encoding stop failed")
	}
	response = do(t, mustRequest(t, http.MethodGet, base+"0.m4s?api_key=gateway-token", nil))
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("encoding stop removed playback")
	}
	if err = store.RevokeSession(context.Background(), HashToken("gateway-token")); err != nil {
		t.Fatal(err)
	}
	response = do(t, mustRequest(t, http.MethodGet, base+"0.m4s?api_key=gateway-token", nil))
	_ = response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("revoked session can read media")
	}
}
