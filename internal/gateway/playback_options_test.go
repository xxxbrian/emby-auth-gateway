package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xxxbrian/emby-auth-gateway/internal/transcode"
)

func TestPlaybackOptionsNormalizeQueryAndJSON(t *testing.T) {
	const body = `{"IsPlayback":"TRUE","EnableDirectStream":"false","AudioStreamIndex":null,"SubtitleStreamIndex":-1,"StartTimeTicks":"9007199254740993","MaxAudioChannels":6,"DeviceProfile":{"MaxStreamingBitrate":"120000000","TranscodingProfiles":[{"Type":"Video","MaxAudioChannels":6}]},"MediaSourceId":"0007","CurrentPlaySessionId":"0015"}`
	r := httptest.NewRequest(http.MethodPost, "/emby/Items/1/PlaybackInfo?IsPlayback=true&MaxAudioChannels=06&EnableDirectStream=False&SubtitleStreamIndex=-01&AudioStreamIndex=0&StartTimeTicks=9007199254740993", strings.NewReader(body))
	r = withPlaybackOptions(r)
	options, _ := r.Context().Value(playbackOptionsKey{}).(*playbackOptions)
	if options == nil || !options.playback || options.start != 9007199254740993 || options.sourceID != "0007" || options.previousID != "0015" {
		t.Fatalf("playback options: %+v", options)
	}
	request := options.request
	if request.AudioStreamIndex == nil || *request.AudioStreamIndex != 0 || request.SubtitleStreamIndex == nil || *request.SubtitleStreamIndex != -1 || request.MaxAudioChannels != 6 {
		t.Fatalf("integer playback options: %+v", request)
	}
	if request.EnableDirectStream == nil || *request.EnableDirectStream || request.EnableTranscoding != nil {
		t.Fatalf("boolean playback options: %+v", request)
	}
	profile := request.Profile
	if profile.MaxStreamingBitrate != 120000000 || len(profile.TranscodingProfiles) != 1 || profile.TranscodingProfiles[0].MaxAudioChannels != "6" {
		t.Fatalf("device profile: %+v", profile)
	}
	got, err := io.ReadAll(r.Body)
	if err != nil || string(got) != body {
		t.Fatalf("proxy request body changed: %q, %v", got, err)
	}
}

func TestNullablePlaybackOptionsUseDefaults(t *testing.T) {
	const body = `{"DeviceProfile":{"MaxStreamingBitrate":null},"AudioStreamIndex":null,"SubtitleStreamIndex":null,"MaxAudioChannels":null,"MaxStreamingBitrate":null,"StartTimeTicks":null,"MediaSourceId":null}`
	options, err := parsePlaybackOptions(http.MethodPost, nil, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	request := options.request
	if request.AudioStreamIndex != nil || request.SubtitleStreamIndex != nil || request.MaxAudioChannels != 0 || request.MaxStreamingBitrate != 0 || options.start != 0 || options.sourceID != "" {
		t.Fatalf("null options changed defaults: %+v", options)
	}
}

func TestPlaybackOptionsReplayUnsupportedRequests(t *testing.T) {
	for _, test := range []struct {
		name  string
		body  string
		query string
	}{
		{name: "ambiguous query and body", body: `{"AudioStreamIndex":2}`, query: "?AudioStreamIndex=1"},
		{name: "duplicate body keys", body: `{"AudioStreamIndex":1,"AudioStreamIndex":2}`},
		{name: "case variant body keys", body: `{"SubtitleStreamIndex":1,"subtitleStreamIndex":-1}`},
		{name: "opaque source ID", body: `{"MediaSourceId":7}`, query: "?MediaSourceId=007"},
		{name: "integer overflow", body: `{"StartTimeTicks":9223372036854775808}`},
		{name: "fractional integer", body: `{"AudioStreamIndex":1.5}`},
		{name: "future profile", body: `{"DeviceProfile":{"CodecProfiles":"future-profile-shape"}}`},
		{name: "invalid JSON", body: `{"DeviceProfile":{}{"StartTimeTicks":1}}`},
		{name: "trailing JSON", body: `{} {}`},
		{name: "oversize profile", body: strings.Repeat("x", playbackOptionsBodyLimit+8)},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/emby/Items/item/PlaybackInfo"+test.query, strings.NewReader(test.body))
			r = withPlaybackOptions(r)
			if r.Context().Value(playbackOptionsKey{}) != nil {
				t.Fatal("unsupported input enabled local negotiation")
			}
			got, err := io.ReadAll(r.Body)
			if err != nil || string(got) != test.body {
				t.Fatalf("replayed request has %d bytes, want %d, err=%v", len(got), len(test.body), err)
			}
		})
	}
}

type resettingPlaybackRequestBody struct{ *strings.Reader }

func (body *resettingPlaybackRequestBody) Read(buffer []byte) (int, error) {
	count, err := body.Reader.Read(buffer)
	if err == io.EOF {
		_, _ = body.Reader.Seek(0, io.SeekStart)
	}
	return count, err
}

func TestPlaybackOptionsReplayPocketBaseBodyExactlyOnce(t *testing.T) {
	const body = `{"DeviceProfile":{},"SubtitleStreamIndex":-1}`
	r := httptest.NewRequest(http.MethodPost, "/emby/Items/item/PlaybackInfo?IsPlayback=true", nil)
	closed := false
	r.Body = closeTrackingReadCloser{Reader: &resettingPlaybackRequestBody{strings.NewReader(body)}, closed: &closed}
	r = withPlaybackOptions(r)
	got, err := io.ReadAll(r.Body)
	if err != nil || string(got) != body {
		t.Fatalf("proxy body duplicated or changed: %q, %v", got, err)
	}
	if err := r.Body.Close(); err != nil || !closed {
		t.Fatalf("proxy did not close original request body: %v", err)
	}
}

func TestPlaybackOptionsReplayBodyReadError(t *testing.T) {
	const body = `{"DeviceProfile":{}}`
	const prefixLength = 5
	r := httptest.NewRequest(http.MethodPost, "/emby/Items/item/PlaybackInfo", &recoveringErrorReader{remaining: []byte(body), first: prefixLength})
	r = withPlaybackOptions(r)
	got, err := io.ReadAll(r.Body)
	if err == nil || string(got) != body[:prefixLength] || r.Context().Value(playbackOptionsKey{}) != nil {
		t.Fatalf("request body error was changed: %q, %v", got, err)
	}
}

func TestGatewayPreservesNativePlaybackNegotiation(t *testing.T) {
	for _, test := range []struct {
		name   string
		query  string
		body   string
		size   string
		status int
	}{
		{name: "empty capabilities", body: `{"DeviceProfile":{}}`, size: `10`, status: 200},
		{name: "new profile shape", body: `{"DeviceProfile":{"CodecProfiles":"future"}}`, size: `10`, status: 200},
		{name: "contradictory options", query: "&AudioStreamIndex=1", body: `{"AudioStreamIndex":2,"DeviceProfile":{}}`, size: `10`, status: 200},
		{name: "unknown media field representation", body: `{"DeviceProfile":{"DirectPlayProfiles":[{"Type":"Video","Container":"mkv"}]}}`, size: `"unknown"`, status: 200},
		{name: "invalid body rejected by Emby", body: `{"DeviceProfile":`, status: 422},
	} {
		t.Run(test.name, func(t *testing.T) {
			received := make(chan string, 1)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				data, err := io.ReadAll(req.Body)
				if err != nil || req.Header.Get("X-Emby-Token") != "backend-token" || req.URL.Query().Get("api_key") != "backend-token" {
					http.Error(w, "invalid upstream request", http.StatusBadRequest)
					return
				}
				received <- string(data)
				if !json.Valid(data) {
					http.Error(w, "upstream validation", http.StatusUnprocessableEntity)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"PlaySessionId":"upstream-play","MediaSources":[{"Id":"0007","Container":"mkv","Size":`+test.size+`,"SupportsDirectPlay":true,"DirectStreamUrl":"/Videos/item/original.mkv?api_key=backend-token","MediaStreams":[{"Index":0,"Type":"Video","Codec":"h264"},{"Index":1,"Type":"Audio","Codec":"aac"}]}]}`)
			}))
			defer backend.Close()
			store := NewMemoryStore()
			configureTestUpstream(store, backend.URL+"/emby")
			store.Sessions[HashToken("gateway-token")] = testSession()
			server := NewServer(Config{GatewayBasePath: "/emby", Transcoder: new(transcode.Manager)}, store)
			r := httptest.NewRequest(http.MethodPost, "/emby/Items/item/PlaybackInfo?IsPlayback=true&api_key=gateway-token"+test.query, strings.NewReader(test.body))
			r.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, r)
			if response.Code != test.status {
				t.Fatalf("status=%d, want %d: %s", response.Code, test.status, response.Body)
			}
			select {
			case body := <-received:
				if body != test.body {
					t.Fatalf("upstream received %q, want %q", body, test.body)
				}
			default:
				t.Fatal("request did not reach Emby")
			}
			if test.status == 200 && (!strings.Contains(response.Body.String(), `"SupportsDirectPlay":true`) ||
				!strings.Contains(response.Body.String(), `"PlaySessionId":"upstream-play"`) ||
				!strings.Contains(response.Body.String(), `api_key=gateway-token`) ||
				strings.Contains(response.Body.String(), `backend-token`)) {
				t.Fatalf("upstream result was not safely preserved: %s", response.Body)
			}
		})
	}
}
