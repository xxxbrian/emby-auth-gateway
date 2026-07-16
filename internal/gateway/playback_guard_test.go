package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentPlaybackDenialNormalizesAndSuppressesReports(t *testing.T) {
	const token = "gateway-token"
	var requests int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("WWW-Authenticate", "Emby")
		w.Header().Set("Content-Encoding", "identity")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"REASON_CODE":"max_concurrent_sessions_exceeded"}`)
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken(token)] = testSession()
	server := NewServer(Config{GatewayBasePath: "/emby"}, store)
	gw := httptest.NewServer(server)
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/item-1/PlaybackInfo?api_key="+token, nil))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || string(body) != concurrentPlaybackResponse {
		t.Fatalf("denial = %d %q", resp.StatusCode, body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") || resp.Header.Get("WWW-Authenticate") != "" || resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("Content-Length") == "999" {
		t.Fatalf("unexpected denial headers: %#v", resp.Header)
	}
	var denial map[string]string
	if err := json.Unmarshal(body, &denial); err != nil || denial["error"] != "playback_access_denied" || denial["reason_code"] != "max_concurrent_sessions_exceeded" || denial["message"] == "" {
		t.Fatalf("invalid denial schema: %#v err=%v", denial, err)
	}
	if requests != 1 {
		t.Fatalf("backend requests = %d, want 1", requests)
	}

	report := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Stopped?api_key="+token, strings.NewReader(`{"ItemId":"item-1","Played":true}`))
	report.Header.Set("Content-Type", "application/json")
	resp = do(t, report)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || len(store.PlaybackEvents) != 0 || len(store.PlaybackStates) != 0 {
		t.Fatalf("suppressed report status/events/states = %d/%d/%d", resp.StatusCode, len(store.PlaybackEvents), len(store.PlaybackStates))
	}
	if !hasAuditEvent(store, "playback_concurrency_denied") || !hasAuditEvent(store, "playback_report_suppressed") {
		t.Fatalf("missing concurrency audits: %#v", store.AuditLogs)
	}
}

func TestPlaybackGuardTrackerGenerationTTLAndCooldown(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tracker := newPlaybackGuardTracker()
	tracker.now = func() time.Time { return now }
	key := playbackGuardKey{GatewayTokenHash: "session", ItemID: "item"}
	if !tracker.deny(key) || tracker.deny(key) {
		t.Fatal("denial audit cooldown was not applied")
	}
	generation := tracker.snapshot(key)
	if generation == 0 {
		t.Fatal("missing guard generation")
	}
	if active, eligible := tracker.suppress(key); !active || !eligible {
		t.Fatalf("first suppression = active %v eligible %v", active, eligible)
	}
	tracker.clearIfGeneration(key, generation)
	if active, _ := tracker.suppress(key); active {
		t.Fatal("matching success did not clear guard")
	}
	if tracker.deny(key) { // Denial cooldown is independent from successful clears.
		t.Fatal("successful clear reset denial audit cooldown")
	}
	if active, eligible := tracker.suppress(key); !active || eligible {
		t.Fatalf("successful clear reset suppression audit cooldown: active=%v eligible=%v", active, eligible)
	}
	now = now.Add(playbackAuditCooldown)
	if !tracker.deny(key) {
		t.Fatal("expired cooldown was not swept")
	}
	oldGeneration := tracker.snapshot(key)
	tracker.deny(key)
	tracker.clearIfGeneration(key, oldGeneration)
	if tracker.snapshot(key) == 0 {
		t.Fatal("old success cleared newer denial")
	}
	now = now.Add(playbackGuardTTL)
	if tracker.snapshot(key) != 0 {
		t.Fatal("expired guard was not swept")
	}
}

func TestConcurrentPlaybackDenialRestoresNonmatchingBodyAndDelegatesClose(t *testing.T) {
	closed := false
	original := closeTrackingReadCloser{Reader: bytes.NewBufferString(`{"reason_code":"other"}`), closed: &closed}
	resp := &http.Response{Body: original}
	if isConcurrentPlaybackDenial(resp) {
		t.Fatal("unexpected denial match")
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil || string(data) != `{"reason_code":"other"}` {
		t.Fatalf("restored body = %q err=%v", data, err)
	}
	if err := resp.Body.Close(); err != nil || !closed {
		t.Fatalf("delegated close err=%v closed=%v", err, closed)
	}
	const trailing = `{"reason_code":"max_concurrent_sessions_exceeded"} trailing`
	resp = &http.Response{Body: io.NopCloser(strings.NewReader(trailing))}
	if isConcurrentPlaybackDenial(resp) {
		t.Fatal("JSON prefix followed by data matched")
	}
	data, err = io.ReadAll(resp.Body)
	if err != nil || string(data) != trailing {
		t.Fatalf("trailing body was not restored: %q err=%v", data, err)
	}
}

func TestConcurrentPlaybackDenialClassifierRestoresInvalidOversizedAndReadErrorBodies(t *testing.T) {
	oversized := strings.Repeat("x", (48<<10)+1)
	tests := []struct {
		name string
		body io.Reader
		want string
	}{
		{name: "invalid JSON", body: strings.NewReader(`{"reason_code":`), want: `{"reason_code":`},
		{name: "oversized", body: strings.NewReader(oversized), want: oversized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			closed := false
			resp := &http.Response{Body: closeTrackingReadCloser{Reader: tt.body, closed: &closed}}
			if isConcurrentPlaybackDenial(resp) {
				t.Fatal("non-complete body was classified as denial")
			}
			data, err := io.ReadAll(resp.Body)
			if err != nil || string(data) != tt.want {
				t.Fatalf("restored body = %d bytes %q, err=%v", len(data), data, err)
			}
			_ = resp.Body.Close()
			if !closed {
				t.Fatal("restored body did not delegate Close")
			}
		})
	}

	const original = `{"reason_code":"max_concurrent_sessions_exceeded"}`
	closed := false
	resp := &http.Response{Body: closeTrackingReadCloser{Reader: &recoveringErrorReader{remaining: []byte(original), first: 9}, closed: &closed}}
	if isConcurrentPlaybackDenial(resp) {
		t.Fatal("partial read error was classified as denial")
	}
	first := make([]byte, len(original))
	n, err := resp.Body.Read(first)
	if got := string(first[:n]); got != original[:9] || err == nil || err.Error() != "temporary read error" {
		t.Fatalf("first restored read = %q, %v", got, err)
	}
	remainder, err := io.ReadAll(resp.Body)
	if err != nil || string(first[:n])+string(remainder) != original {
		t.Fatalf("restored error body = %q + %q, err=%v", first[:n], remainder, err)
	}
	_ = resp.Body.Close()
	if !closed {
		t.Fatal("error-restored body did not delegate Close")
	}
}

func TestConcurrentPlaybackDenialRejectsDuplicateReasonCodeKeys(t *testing.T) {
	tests := []string{
		`{"reason_code":"max_concurrent_sessions_exceeded","REASON_CODE":"other"}`,
		`{"reason_code":"max_concurrent_sessions_exceeded","REASON_CODE":"max_concurrent_sessions_exceeded"}`,
	}
	for _, body := range tests {
		for range 20 {
			resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
			if isConcurrentPlaybackDenial(resp) {
				t.Fatalf("duplicate reason_code keys matched: %s", body)
			}
			data, err := io.ReadAll(resp.Body)
			if err != nil || string(data) != body {
				t.Fatalf("duplicate body was not restored: %q err=%v", data, err)
			}
		}
	}
}

func TestPlaybackInfoReadErrorReachesProxyResponseWithoutGuard(t *testing.T) {
	const token = "gateway-token"
	const body = `{"reason_code":"max_concurrent_sessions_exceeded"}`
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/emby/Items/item-1/PlaybackInfo":
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(&recoveringErrorReader{remaining: []byte(body), first: 9}),
				Request:    req,
			}, nil
		case "/emby/System/Info":
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Request: req}, nil
		default:
			return nil, errors.New("unexpected transport request: " + req.URL.Path)
		}
	})
	store := NewMemoryStore()
	configureTestUpstream(store, "http://backend/emby")
	store.Sessions[HashToken(token)] = testSession()
	server := NewServer(Config{GatewayBasePath: "/emby", HTTPClient: &http.Client{Transport: transport}}, store)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, mustRequest(t, http.MethodGet, "http://gateway/emby/Items/item-1/PlaybackInfo?api_key="+token, nil))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("read-error proxy status = %d, want 502", recorder.Code)
	}
	if server.playbackGuards.snapshot(playbackGuardKey{GatewayTokenHash: HashToken(token), ItemID: "item-1"}) != 0 {
		t.Fatal("read-error response created a playback guard")
	}
	if !hasAuditEvent(store, "proxy_read_failed") {
		t.Fatalf("missing proxy_read_failed audit: %#v", store.AuditLogs)
	}
}

func TestPlaybackInfoItemID(t *testing.T) {
	tests := []struct {
		method, rel, item string
		ok                bool
	}{
		{http.MethodGet, "/Items/item/PlaybackInfo", "item", true},
		{http.MethodPost, "/items/item/playbackinfo/", "item", true},
		{http.MethodGet, "/Items//PlaybackInfo", "", false},
		{http.MethodGet, "/Items/item/PlaybackInfo/extra", "", false},
		{http.MethodGet, "/Items/item/PlaybackInfos", "", false},
		{http.MethodDelete, "/Items/item/PlaybackInfo", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.method+tt.rel, func(t *testing.T) {
			item, ok := playbackInfoItemID(tt.method, tt.rel)
			if item != tt.item || ok != tt.ok {
				t.Fatalf("playbackInfoItemID = %q, %v; want %q, %v", item, ok, tt.item, tt.ok)
			}
		})
	}
}

func TestPlaybackGuardTrackerSweepsOtherExpiredKeys(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tracker := newPlaybackGuardTracker()
	tracker.now = func() time.Time { return now }
	expired := playbackGuardKey{GatewayTokenHash: "expired", ItemID: "item"}
	live := playbackGuardKey{GatewayTokenHash: "live", ItemID: "item"}
	tracker.guards[expired] = playbackGuard{generation: 99, expiresAt: now.Add(-time.Second)}
	tracker.cooldowns[playbackAuditKey{guard: expired, event: "event"}] = now.Add(-time.Second)
	tracker.guards[live] = playbackGuard{generation: 100, expiresAt: now.Add(time.Hour)}
	tracker.snapshot(live)
	if _, ok := tracker.guards[expired]; ok {
		t.Fatal("snapshot did not sweep another key's expired guard")
	}
	if _, ok := tracker.cooldowns[playbackAuditKey{guard: expired, event: "event"}]; ok {
		t.Fatal("snapshot did not sweep another key's expired cooldown")
	}
}

func TestPlaybackInfoRefreshRetryConcurrentDenial(t *testing.T) {
	const token = "gateway-token"
	var logins, playbackCalls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Items/item-1/PlaybackInfo":
			if playbackCalls.Add(1) == 1 {
				http.Error(w, "stale", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"reason_code":"max_concurrent_sessions_exceeded"}`)
		case "/emby/System/Info":
			http.Error(w, "stale", http.StatusUnauthorized)
		case "/emby/Users/AuthenticateByName":
			logins.Add(1)
			writeTestJSON(w, map[string]any{"AccessToken": "new-token", "ServerId": "backend-server", "User": map[string]any{"Id": "backend-user"}})
		case "/emby/Sessions/Logout":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected backend request %s", r.URL.Path)
		}
	}))
	defer backend.Close()
	store := testStore(backend.URL + "/emby")
	source := store.UpstreamSources["source"]
	source.BackendToken = "old-token"
	store.UpstreamSources["source"] = source
	session := testSession()
	store.Sessions[HashToken(token)] = session
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()
	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/item-1/PlaybackInfo?api_key="+token, nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || logins.Load() != 1 || playbackCalls.Load() != 2 {
		t.Fatalf("status/logins/playbacks = %d/%d/%d", resp.StatusCode, logins.Load(), playbackCalls.Load())
	}
}

type closeTrackingReadCloser struct {
	io.Reader
	closed *bool
}

func (r closeTrackingReadCloser) Close() error {
	*r.closed = true
	return nil
}

type recoveringErrorReader struct {
	remaining []byte
	first     int
	failed    bool
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func (r *recoveringErrorReader) Read(p []byte) (int, error) {
	if len(r.remaining) == 0 {
		return 0, io.EOF
	}
	n := len(r.remaining)
	if !r.failed && r.first < n {
		n = r.first
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, r.remaining[:n])
	r.remaining = r.remaining[n:]
	if !r.failed {
		r.failed = true
		return n, errors.New("temporary read error")
	}
	return n, nil
}
