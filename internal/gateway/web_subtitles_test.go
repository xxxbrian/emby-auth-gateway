package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/subtitles"
	"github.com/xxxbrian/emby-auth-gateway/internal/transcode"
	"github.com/xxxbrian/emby-auth-gateway/internal/webcontext"
)

const captionVTT = "WEBVTT\n\n00:00:01.000 --> 00:00:03.000\nA working subtitle\n"

type subtitleHarness struct {
	server     *httptest.Server
	manager    *subtitles.Manager
	store      *MemoryStore
	marker     *webcontext.Manager
	cookie     *http.Cookie
	probes     atomic.Int32
	reads      atomic.Int32
	cookieLeak atomic.Bool
	requests   atomic.Int32
}

func newSubtitleHarness(t *testing.T, enabled, unavailable bool) *subtitleHarness {
	t.Helper()
	h := &subtitleHarness{}
	media := func() map[string]any {
		return map[string]any{"Id": "source", "Container": "mkv", "Size": 16, "RunTimeTicks": 600000000,
			"SupportsDirectPlay": true, "SupportsDirectStream": true, "SupportsTranscoding": false, "DefaultSubtitleStreamIndex": 2,
			"DirectStreamUrl": "/Videos/item/original.mkv?api_key=backend-token", "HasSubtitles": true,
			"MediaStreams": []any{
				map[string]any{"Type": "Video", "Index": 0, "Codec": "h264"},
				map[string]any{"Type": "Audio", "Index": 1, "Codec": "aac", "Channels": 2},
				map[string]any{"Type": "Subtitle", "Index": 2, "Codec": "subrip", "Language": "eng", "DeliveryMethod": "External", "DeliveryUrl": "/Videos/item/source/Subtitles/2/0/Stream.vtt?api_key=backend-token"},
				map[string]any{"Type": "Subtitle", "Index": 3, "Codec": "vtt", "Language": "eng", "IsExternal": true, "DeliveryMethod": "External", "DeliveryUrl": "/Videos/item/source/Subtitles/3/0/Stream.vtt?api_key=backend-token"},
			}}
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		if strings.Contains(r.Header.Get("Cookie"), webcontext.CookieName) {
			h.cookieLeak.Store(true)
		}
		switch {
		case strings.Contains(r.URL.Path, "/Subtitles/"):
			h.probes.Add(1)
			if r.Header.Get("X-Emby-Token") != "backend-token" || r.URL.Query().Get("api_key") != "backend-token" {
				http.Error(w, "invalid backend credentials", 401)
				return
			}
			w.Header().Set("Content-Type", "text/vtt")
			if strings.Contains(r.URL.Path, "/3/") {
				_, _ = io.WriteString(w, captionVTT)
			}
		case strings.HasSuffix(r.URL.Path, "/original.mkv"):
			h.reads.Add(1)
			w.Header().Set("ETag", `"source-one"`)
			http.ServeContent(w, r, "source.mkv", time.Time{}, strings.NewReader("invalid-MKV-data"))
		case strings.HasSuffix(r.URL.Path, "/PlaybackInfo"):
			writeTestJSON(w, map[string]any{"PlaySessionId": "play", "MediaSources": []any{media()}})
		default:
			m := media()
			writeTestJSON(w, map[string]any{"Id": "item", "Type": "Movie", "Name": "Example", "HasSubtitles": true, "MediaSources": []any{m}, "MediaStreams": m["MediaStreams"]})
		}
	}))
	t.Cleanup(backend.Close)
	h.store = NewMemoryStore()
	configureTestUpstream(h.store, backend.URL+"/emby")
	for token, client := range map[string]string{"web-token": "Emby Web", "native-token": "SenPlayer", "unknown-token": ""} {
		session := testSession()
		session.GatewayTokenHash, session.Client, session.DeviceID = HashToken(token), client, "device"
		h.store.Sessions[HashToken(token)] = session
	}
	var err error
	h.marker, err = webcontext.New()
	if err != nil {
		t.Fatal(err)
	}
	if enabled && !unavailable {
		h.manager, err = subtitles.New(subtitles.Config{Dir: t.TempDir(), PrepareTimeout: 100 * time.Millisecond, WorkTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.manager.Close() })
	}
	gw := NewServer(Config{GatewayBasePath: "/emby", Subtitles: h.manager, WebContext: h.marker, WebSubtitlesEnabled: enabled}, h.store)
	h.server = httptest.NewServer(gw)
	t.Cleanup(h.server.Close)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, h.server.URL+"/emby/web/", nil)
	h.marker.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html></html>")
	})).ServeHTTP(w, r)
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == webcontext.CookieName {
			h.cookie = cookie
		}
	}
	if h.cookie == nil {
		t.Fatal("missing Web context")
	}
	return h
}

func cachedSubtitleFixture(t *testing.T) (*subtitleHarness, *http.Request, subtitles.Input) {
	t.Helper()
	h := newSubtitleHarness(t, true, false)
	var caption string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		media := h.info(t, "web-token", h.cookie, "")
		for _, raw := range media["MediaStreams"].([]any) {
			stream := raw.(map[string]any)
			if stream["Type"] == "Subtitle" && stream["Index"] == float64(3) {
				caption, _ = stream["DeliveryUrl"].(string)
			}
		}
		if caption != "" && !h.manager.HasActiveWork() {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if caption == "" {
		t.Fatal("caption was not prepared")
	}
	original := h.info(t, "native-token", nil, "")
	data, _ := json.Marshal(original)
	var media transcode.MediaSource
	if err := json.Unmarshal(data, &media); err != nil {
		t.Fatal(err)
	}
	runtime, err := h.store.LoadDefaultUpstreamRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := upstreamRequestSnapshotFromRuntime(runtime)
	if err != nil {
		t.Fatal(err)
	}
	session, err := h.store.FindSessionByTokenHash(context.Background(), HashToken("web-token"))
	if err != nil {
		t.Fatal(err)
	}
	gateway := h.server.Config.Handler.(*Server)
	in := gateway.webSubtitleInput(httptest.NewRequest("GET", h.server.URL+"/emby/Items/item", nil), original, media, "item", "play", session, upstream, "web-token")
	req := mustRequest(t, http.MethodGet, h.server.URL+"/emby"+caption, nil)
	req.AddCookie(h.cookie)
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cached caption initial status=%d", resp.StatusCode)
	}
	return h, req, in
}

func TestCachedWebSubtitleRechecksOriginalPolicyAndUpstreamBinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*MemoryStore)
		want   int
	}{
		{"original subtitle path denied", func(store *MemoryStore) {
			store.PathPolicies = []PathPolicy{{Method: http.MethodGet, Path: "/Videos/item/source/Subtitles/3/0/Stream.vtt", Action: "deny", Enabled: true}}
		}, http.StatusNotFound},
		{"unrelated subtitle path denied", func(store *MemoryStore) {
			store.PathPolicies = []PathPolicy{{Method: http.MethodGet, Path: "/Videos/item/source/Subtitles/2/0/Stream.vtt", Action: "deny", Enabled: true}}
		}, http.StatusOK},
		{"backend identity replaced", func(store *MemoryStore) {
			source := store.UpstreamSources["source"]
			source.BackendUserID = "different-user"
			store.UpstreamSources["source"] = source
		}, http.StatusNotFound},
		{"server identity replaced", func(store *MemoryStore) {
			source := store.UpstreamSources["source"]
			source.ServerID = "different-server"
			store.UpstreamSources["source"] = source
		}, http.StatusNotFound},
		{"endpoint replaced", func(store *MemoryStore) {
			endpoint := store.UpstreamEndpoints["endpoint"]
			endpoint.BaseURL = "http://127.0.0.1:1/emby"
			store.UpstreamEndpoints["endpoint"] = endpoint
		}, http.StatusNotFound},
		{"original caption route replaced", func(store *MemoryStore) {
			store.UpstreamEndpoints["alternate"] = UpstreamEndpoint{ID: "alternate", SourceID: "source", Key: "alternate", BaseURL: "http://127.0.0.1:1/emby", Enabled: true}
			store.RouteRules = []RouteRule{{Method: http.MethodGet, Path: "/Videos/item/source/Subtitles/3/0/Stream.vtt", Target: "alternate", Enabled: true}}
		}, http.StatusNotFound},
		{"credentials rotate without identity change", func(store *MemoryStore) {
			source := store.UpstreamSources["source"]
			source.BackendToken = "rotated-token"
			source.AuthGenerationID = "new-generation"
			source.ClientIdentity.DeviceID = "rotated-device"
			store.UpstreamSources["source"] = source
		}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, req, in := cachedSubtitleFixture(t)
			before := h.requests.Load()
			h.store.mu.Lock()
			tc.change(h.store)
			h.store.mu.Unlock()
			resp := do(t, req)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("cached caption status=%d want=%d", resp.StatusCode, tc.want)
			}
			ready := h.manager.Prepare(context.Background(), in, subtitles.Selection{})
			if (len(ready) == 1) != (tc.want == http.StatusOK) {
				t.Fatalf("ready projection after permission change=%#v", ready)
			}
			if h.requests.Load() != before {
				t.Fatal("cached permission validation probed the origin")
			}
		})
	}
}

func (h *subtitleHarness) info(t *testing.T, token string, cookie *http.Cookie, extra string) map[string]any {
	t.Helper()
	req := mustRequest(t, http.MethodPost, h.server.URL+"/emby/Items/item/PlaybackInfo?IsPlayback=true&SubtitleStreamIndex=2&api_key="+token+extra, strings.NewReader(`{"DeviceProfile":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 Chrome")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("PlaybackInfo status: %d", resp.StatusCode)
	}
	var value map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value["MediaSources"].([]any)[0].(map[string]any)
}

func TestWebSubtitleNativeAndDisabledPathsDoNoWork(t *testing.T) {
	for _, tc := range []struct {
		name, token, extra string
		enabled, cookie    bool
	}{
		{"disabled Web", "web-token", "", false, true},
		{"native", "native-token", "", true, false},
		{"native with cookie", "native-token", "", true, true},
		{"native claim override", "native-token", "&X-Emby-Client=Emby+Web", true, true},
		{"unknown with cookie", "unknown-token", "", true, true},
		{"Web without context", "web-token", "", true, false},
		{"Web different device", "web-token", "&X-Emby-Device-Id=another", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSubtitleHarness(t, tc.enabled, false)
			var cookie *http.Cookie
			if tc.cookie {
				cookie = h.cookie
			}
			m := h.info(t, tc.token, cookie, tc.extra)
			if len(m["MediaStreams"].([]any)) != 4 || m["DefaultSubtitleStreamIndex"] != float64(2) || m["SupportsDirectPlay"] != true {
				t.Fatalf("original client metadata changed: %+v", m)
			}
			if h.probes.Load() != 0 || h.reads.Load() != 0 || h.cookieLeak.Load() {
				t.Fatalf("unexpected work or cookie forwarding: probes=%d reads=%d leak=%v", h.probes.Load(), h.reads.Load(), h.cookieLeak.Load())
			}
			if h.manager != nil && len(h.manager.Snapshot().Jobs) != 0 {
				t.Fatal("native/disabled request registered subtitle work")
			}
		})
	}
}

func TestWebSubtitlesReadyOnlyAndNativeMetadataIsolation(t *testing.T) {
	h := newSubtitleHarness(t, true, false)
	var m map[string]any
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m = h.info(t, "web-token", h.cookie, "")
		if len(m["MediaStreams"].([]any)) == 3 {
			break
		}
	}
	streams := m["MediaStreams"].([]any)
	if len(streams) != 3 || m["DefaultSubtitleStreamIndex"] != float64(-1) || m["SupportsDirectPlay"] != true {
		t.Fatalf("unavailable caption affected playback: %+v", m)
	}
	caption := streams[2].(map[string]any)
	if caption["Index"] != float64(3) {
		t.Fatal("subtitle track index was renumbered")
	}
	path := caption["DeliveryUrl"].(string)
	if !strings.Contains(path, "/gateway-subtitles/") {
		t.Fatal("unverified upstream URL was advertised")
	}
	req := mustRequest(t, http.MethodGet, h.server.URL+"/emby"+path, nil)
	req.AddCookie(h.cookie)
	resp := do(t, req)
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(data) != captionVTT {
		t.Fatalf("ready subtitle: %d %q", resp.StatusCode, data)
	}
	before := h.probes.Load()
	original := h.info(t, "native-token", h.cookie, "")
	if len(original["MediaStreams"].([]any)) != 4 || h.probes.Load() != before {
		t.Fatal("shared Web availability changed native metadata or caused work")
	}
	if h.cookieLeak.Load() {
		t.Fatal("Web context reached origin")
	}
	if err := h.store.RevokeSession(context.Background(), HashToken("web-token")); err != nil {
		t.Fatal(err)
	}
	resp = do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("revoked session can access a prepared caption")
	}
}

func TestUnavailableSubtitleRuntimeHidesWebTracksWithoutWork(t *testing.T) {
	h := newSubtitleHarness(t, true, true)
	m := h.info(t, "web-token", h.cookie, "")
	if len(m["MediaStreams"].([]any)) != 2 || m["HasSubtitles"] != false || m["DefaultSubtitleStreamIndex"] != float64(-1) || m["SupportsDirectPlay"] != true {
		t.Fatalf("failed optional runtime affected main playback: %+v", m)
	}
	if h.reads.Load() != 0 || h.probes.Load() != 0 {
		t.Fatal("unavailable module performed work")
	}
	if native := h.info(t, "native-token", nil, ""); len(native["MediaStreams"].([]any)) != 4 {
		t.Fatal("unavailable module filtered native metadata")
	}
}

func TestSubtitleSourceIdentityIgnoresTransportAndWebProjection(t *testing.T) {
	upstream := testUpstreamSnapshot("https://upstream.invalid/emby")
	media := transcode.MediaSource{ID: "source", Size: 123456, Container: "mkv", RunTimeTicks: 123000000,
		MediaStreams: []transcode.Stream{{Type: "Video", Index: 0, Codec: "h264"}, {Type: "Subtitle", Index: 1, Codec: "subrip", DeliveryURL: "/one?token=secret"}}}
	want := subtitleSourceKey(upstream, "item", media)
	media.DirectStreamURL = "/rotated?api_key=another"
	media.MediaStreams = media.MediaStreams[:1]
	if got := subtitleSourceKey(upstream, "item", media); got != want {
		t.Fatal("token rotation or Web subtitle filtering changed raw source identity")
	}
	media.Size++
	if got := subtitleSourceKey(upstream, "item", media); got == want {
		t.Fatal("source replacement retained identity")
	}
}

func TestOptionalSubtitleParsingPreservesUnsupportedRequests(t *testing.T) {
	for _, body := range []string{
		`{"DeviceProfile":{},"SubtitleStreamIndex":null}`,
		`{"DeviceProfile":{"CodecProfiles":"future-profile-shape"}}`,
		strings.Repeat("x", (2<<20)+8),
	} {
		r := httptest.NewRequest(http.MethodPost, "/emby/Items/item/PlaybackInfo", strings.NewReader(body))
		parsed := withOptionalSubtitleNegotiation(r)
		got, err := io.ReadAll(parsed.Body)
		if err != nil || string(got) != body {
			t.Fatal("optional parsing changed the proxy body")
		}
		if invalid, _ := parsed.Context().Value(subtitleOptionsInvalidKey{}).(bool); !invalid {
			t.Fatal("unsupported options should disable subtitle preparation")
		}
	}
	h := newSubtitleHarness(t, true, false)
	req := mustRequest(t, http.MethodPost, h.server.URL+"/emby/Items/item/PlaybackInfo?IsPlayback=true&api_key=web-token", strings.NewReader(`{"DeviceProfile":{},"SubtitleStreamIndex":null}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(h.cookie)
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || h.probes.Load() != 0 || h.reads.Load() != 0 {
		t.Fatal("optional parser introduced a proxy failure or preparation")
	}
}

type resettingSubtitleRequestBody struct{ *strings.Reader }

func (b *resettingSubtitleRequestBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		_, _ = b.Reader.Seek(0, io.SeekStart)
	}
	return n, err
}

func (*resettingSubtitleRequestBody) Close() error { return nil }

func TestOptionalSubtitleParsingReplaysPocketBaseBodyExactlyOnce(t *testing.T) {
	const body = `{"DeviceProfile":{},"SubtitleStreamIndex":-1}`
	r := httptest.NewRequest(http.MethodPost, "/emby/Items/item/PlaybackInfo?IsPlayback=true", nil)
	r.Body = &resettingSubtitleRequestBody{strings.NewReader(body)}
	r = withOptionalSubtitleNegotiation(r)
	parsed, err := withAudioNegotiation(r)
	if err != nil {
		t.Fatalf("audio negotiation failed after optional preparation: %v", err)
	}
	data, err := io.ReadAll(parsed.Body)
	if err != nil || string(data) != body {
		t.Fatalf("proxy body duplicated/changed: %q %v", data, err)
	}
}

func TestSubtitleValidatorDoesNotReuseMissingETag(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("ETag", `"first-version"`)
		}
		http.ServeContent(w, r, "source.mkv", time.Time{}, bytes.NewReader([]byte("01234567")))
	}))
	defer backend.Close()
	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	s := NewServer(Config{}, store)
	media := transcode.MediaSource{ID: "source", Size: 8, Container: "mkv", DirectStreamURL: "/Videos/item/original.mkv?api_key=backend-token"}
	in := s.webSubtitleInput(httptest.NewRequest("GET", "http://gateway/emby", nil), map[string]any{"MediaStreams": []any{}}, media, "item", "play", testSession(), testUpstreamSnapshot(backend.URL+"/emby"), "gateway-token")
	version, strong, err := in.Validate(context.Background())
	if err != nil || !strong || version != `"first-version"` {
		t.Fatalf("initial validator: %q %v %v", version, strong, err)
	}
	reader, err := in.Open(context.Background(), 0, 4)
	if reader != nil {
		_ = reader.Close()
	}
	if !errors.Is(err, transcode.ErrSource) {
		t.Fatal("indexed reads accepted a response that lost its bound validator")
	}
	version, strong, err = in.Validate(context.Background())
	if err != nil || strong || version == `"first-version"` {
		t.Fatalf("missing validator reused old strong ETag: %q %v %v", version, strong, err)
	}
}

func TestExplicitSubtitleOffWithoutDeviceProfileDoesNotExtract(t *testing.T) {
	for _, query := range []bool{false, true} {
		h := newSubtitleHarness(t, true, false)
		body, extra := `{"SubtitleStreamIndex":-1}`, ""
		if query {
			body, extra = `{}`, "&SubtitleStreamIndex=-1"
		}
		req := mustRequest(t, http.MethodPost, h.server.URL+"/emby/Items/item/PlaybackInfo?IsPlayback=true&api_key=web-token"+extra, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(h.cookie)
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status=%d", resp.StatusCode)
		}
		if err := h.manager.Close(); err != nil {
			t.Fatal(err)
		}
		if h.reads.Load() != 0 {
			t.Fatal("explicit Off caused original-media reads without a DeviceProfile")
		}
	}
}
