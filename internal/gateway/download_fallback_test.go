package gateway

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDownloadForbiddenFallsBackToOfficialPlaybackInfoDirectStream(t *testing.T) {
	var downloadCalls, playbackCalls, mediaCalls int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Items/item-1/Download":
			downloadCalls++
			if r.Header.Get("X-Emby-Token") != "backend-token" || r.URL.Query().Get("api_key") != "backend-token" || r.URL.Query().Get("MediaSourceId") != "source-1" {
				t.Fatalf("native download auth/query = %q/%q", r.Header.Get("X-Emby-Token"), r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "download forbidden")
		case "/emby/Items/item-1/PlaybackInfo":
			playbackCalls++
			if r.Method != http.MethodPost || r.Header.Get("X-Emby-Token") != "backend-token" || r.Header.Get("Range") != "" {
				t.Fatalf("PlaybackInfo method/token/range = %s/%q/%q", r.Method, r.Header.Get("X-Emby-Token"), r.Header.Get("Range"))
			}
			var request embyPlaybackInfoRequestDTO
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.ID != "item-1" || request.UserID != "backend-user" || request.MediaSourceID != "source-1" || !request.EnableDirectPlay || !request.EnableDirectStream || request.EnableTranscoding || request.IsPlayback {
				t.Fatalf("PlaybackInfoRequest = %#v", request)
			}
			writeTestJSON(w, embyPlaybackInfoResponseDTO{
				PlaySessionID: "play-session",
				MediaSources: []embyMediaSourceInfoDTO{{
					ID:                   "source-1",
					Name:                 "电影/标题",
					Container:            "mkv",
					DirectStreamURL:      "/Videos/item-1/original.mkv?MediaSourceId=source-1&PlaySessionId=play-session&sig=a%2Bb&api_key=backend-token",
					SupportsDirectPlay:   true,
					SupportsDirectStream: true,
					RequiredHTTPHeaders:  map[string]string{"X-Media-Source": "required", "Range": "bytes=0-3"},
				}},
			})
		case "/emby/Videos/item-1/original.mkv":
			mediaCalls++
			if r.Method != http.MethodGet || r.Header.Get("X-Emby-Token") != "backend-token" || r.Header.Get("Range") != "bytes=0-" || r.Header.Get("If-Range") != `"tag"` || r.Header.Get("X-Media-Source") != "required" {
				t.Fatalf("media method/headers = %s %#v", r.Method, r.Header)
			}
			q := r.URL.Query()
			if q.Get("MediaSourceId") != "source-1" || q.Get("PlaySessionId") != "play-session" || q.Get("sig") != "a+b" || q.Get("api_key") != "backend-token" {
				t.Fatalf("media query = %q", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "video/x-matroska")
			w.Header().Set("Content-Range", "bytes 0-3/4")
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("ETag", `"tag"`)
			w.Header().Set("Content-Length", "4")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "data")
		default:
			t.Fatalf("unexpected backend request %s %s", r.Method, r.URL.String())
		}
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	gateway := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gateway.Close()

	request := mustRequest(t, http.MethodGet, gateway.URL+"/emby/Items/item-1/Download?MediaSourceId=source-1&api_key=gateway-token", nil)
	request.Header.Set("Range", "bytes=0-")
	request.Header.Set("If-Range", `"tag"`)
	response := do(t, request)
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()

	if response.StatusCode != http.StatusPartialContent || string(body) != "data" || downloadCalls != 1 || playbackCalls != 1 || mediaCalls != 1 {
		t.Fatalf("response/calls = %d %q / %d %d %d", response.StatusCode, body, downloadCalls, playbackCalls, mediaCalls)
	}
	if response.Header.Get("Content-Range") != "bytes 0-3/4" || response.Header.Get("Accept-Ranges") != "bytes" || response.Header.Get("Cache-Control") != "private" {
		t.Fatalf("range/cache headers = %#v", response.Header)
	}
	disposition, params, err := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || params["filename"] != "电影_标题.mkv" {
		t.Fatalf("Content-Disposition = %q (%q %#v %v)", response.Header.Get("Content-Disposition"), disposition, params, err)
	}
}

func TestDownloadFallbackPreservesOriginalForbiddenWhenUnavailable(t *testing.T) {
	tests := []struct {
		name       string
		playback   embyPlaybackInfoResponseDTO
		mediaCalls int
	}{
		{
			name: "missing requested source",
			playback: embyPlaybackInfoResponseDTO{MediaSources: []embyMediaSourceInfoDTO{{
				ID: "other", DirectStreamURL: "/Videos/item-1/original.mkv", SupportsDirectStream: true,
			}}},
		},
		{
			name: "mismatched item url",
			playback: embyPlaybackInfoResponseDTO{MediaSources: []embyMediaSourceInfoDTO{{
				ID: "source-1", DirectStreamURL: "/Videos/item-2/original.mkv?MediaSourceId=source-1", SupportsDirectStream: true,
			}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var playbackCalls, mediaCalls int
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/emby/Items/item-1/Download":
					w.Header().Set("Content-Type", "text/plain")
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, "original forbidden")
				case "/emby/Items/item-1/PlaybackInfo":
					playbackCalls++
					writeTestJSON(w, tt.playback)
				default:
					mediaCalls++
					http.Error(w, "unexpected media", http.StatusInternalServerError)
				}
			}))
			defer backend.Close()
			store := NewMemoryStore()
			configureTestUpstream(store, backend.URL+"/emby")
			store.Sessions[HashToken("gateway-token")] = testSession()
			gateway := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
			defer gateway.Close()

			response := do(t, mustRequest(t, http.MethodGet, gateway.URL+"/emby/Items/item-1/Download?MediaSourceId=source-1&api_key=gateway-token", nil))
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusForbidden || strings.TrimSpace(string(body)) != "original forbidden" || playbackCalls != 1 || mediaCalls != 0 {
				t.Fatalf("response/calls = %d %q / %d %d", response.StatusCode, body, playbackCalls, mediaCalls)
			}
		})
	}
}

func TestDownloadNonForbiddenDoesNotResolvePlaybackInfo(t *testing.T) {
	var playbackCalls int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/PlaybackInfo") {
			playbackCalls++
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "native")
	}))
	defer backend.Close()
	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	gateway := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gateway.Close()

	response := do(t, mustRequest(t, http.MethodGet, gateway.URL+"/emby/Items/item-1/Download?api_key=gateway-token", nil))
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "native" || playbackCalls != 0 {
		t.Fatalf("response/playback calls = %d %q / %d", response.StatusCode, body, playbackCalls)
	}
}
