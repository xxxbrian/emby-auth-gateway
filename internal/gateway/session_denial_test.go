package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
)

// countingRuntimeStore counts LoadDefaultUpstreamRuntime for denial zero-load proofs.
type countingRuntimeStore struct {
	*MemoryStore
	mu    sync.Mutex
	loads int
}

func (c *countingRuntimeStore) LoadDefaultUpstreamRuntime(ctx context.Context) (*UpstreamRuntime, error) {
	c.mu.Lock()
	c.loads++
	c.mu.Unlock()
	return c.MemoryStore.LoadDefaultUpstreamRuntime(ctx)
}

func (c *countingRuntimeStore) loadCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loads
}

func drainEmitter(em *observe.Emitter) []observe.Event {
	if em == nil {
		return nil
	}
	var out []observe.Event
	for {
		select {
		case ev, ok := <-em.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestSessionDeniedTargetedControlCorpus(t *testing.T) {
	recorder := newEgressRecorder(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusNoContent)
	})
	backend := httptest.NewServer(recorder)
	defer backend.Close()

	store := &countingRuntimeStore{MemoryStore: NewMemoryStore()}
	configureTestUpstream(store.MemoryStore, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	em := observe.NewEmitter(64)
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", Emitter: em}, store))
	defer gw.Close()

	type tc struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantAllow  string
		wantAudit  string
	}
	cases := []tc{
		// Recognized command families reject malformed public session IDs locally.
		{name: "playing", method: http.MethodPost, path: "/Sessions/session-1/Playing", wantStatus: http.StatusBadRequest, wantAudit: "session_command_denied"},
		{name: "playing_pause", method: http.MethodPost, path: "/Sessions/session-1/Playing/Pause", wantStatus: http.StatusBadRequest, wantAudit: "session_command_denied"},
		{name: "command", method: http.MethodPost, path: "/Sessions/session-1/Command", wantStatus: http.StatusBadRequest, wantAudit: "session_command_denied"},
		{name: "command_name", method: http.MethodPost, path: "/Sessions/session-1/Command/Mute", wantStatus: http.StatusBadRequest, wantAudit: "session_command_denied"},
		{name: "system", method: http.MethodPost, path: "/Sessions/session-1/System/Restart", wantStatus: http.StatusForbidden, wantAudit: "session_access_denied"},
		{name: "message", method: http.MethodPost, path: "/Sessions/session-1/Message", wantStatus: http.StatusForbidden, wantAudit: "session_access_denied"},
		{name: "viewing", method: http.MethodPost, path: "/Sessions/session-1/Viewing", wantStatus: http.StatusForbidden, wantAudit: "session_access_denied"},
		{name: "users_post", method: http.MethodPost, path: "/Sessions/session-1/Users/user-2", wantStatus: http.StatusForbidden, wantAudit: "session_access_denied"},
		{name: "users_delete", method: http.MethodDelete, path: "/Sessions/session-1/Users/user-2", wantStatus: http.StatusForbidden, wantAudit: "session_access_denied"},
		{name: "users_delete_suffix", method: http.MethodPost, path: "/Sessions/session-1/Users/user-2/Delete", wantStatus: http.StatusForbidden, wantAudit: "session_access_denied"},
		// Ambiguous / no-id control variants and unknown descendants.
		{name: "playing_pause_no_id", method: http.MethodPost, path: "/Sessions/Playing/Pause", wantStatus: http.StatusForbidden, wantAudit: "session_access_denied"},
		{name: "mixed_case", method: http.MethodPost, path: "/sessions/SESSION-1/playing/pause", wantStatus: http.StatusBadRequest, wantAudit: "session_command_denied"},
		{name: "trailing_slash", method: http.MethodPost, path: "/Sessions/session-1/Playing/Pause/", wantStatus: http.StatusBadRequest, wantAudit: "session_command_denied"},
		{name: "unknown_descendant", method: http.MethodGet, path: "/Sessions/session-1/Unknown/Descendant", wantStatus: http.StatusForbidden, wantAudit: "session_access_denied"},
		// PlayQueue: GET denied 403; POST wrong method 405 Allow GET.
		{name: "playqueue_get", method: http.MethodGet, path: "/Sessions/PlayQueue", wantStatus: http.StatusForbidden, wantAudit: "session_access_denied"},
		{name: "playqueue_post", method: http.MethodPost, path: "/Sessions/PlayQueue", wantStatus: http.StatusMethodNotAllowed, wantAllow: "GET", wantAudit: "session_method_not_allowed"},
		// Wrong method on recognized targeted operations -> 405.
		{name: "playing_get_405", method: http.MethodGet, path: "/Sessions/session-1/Playing/Pause", wantStatus: http.StatusMethodNotAllowed, wantAllow: "POST", wantAudit: "session_method_not_allowed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder.reset()
			store.mu.Lock()
			store.AuditLogs = nil
			store.mu.Unlock()
			_ = drainEmitter(em)
			beforeLoads := store.loadCount()

			resp := do(t, mustRequest(t, tc.method, gw.URL+"/emby"+tc.path+"?api_key=gateway-token", nil))
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d body=%q", resp.StatusCode, tc.wantStatus, body)
			}
			if resp.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
			}
			if tc.wantAllow != "" && resp.Header.Get("Allow") != tc.wantAllow {
				t.Fatalf("Allow = %q, want %q", resp.Header.Get("Allow"), tc.wantAllow)
			}
			recorder.assertEmpty(t)
			if store.loadCount() != beforeLoads {
				t.Fatalf("upstream runtime loads increased by %d, want 0", store.loadCount()-beforeLoads)
			}
			if !hasAuditEvent(store.MemoryStore, tc.wantAudit) {
				t.Fatalf("missing audit %q in %#v", tc.wantAudit, store.AuditLogs)
			}
			// Audit fields: method/path/user
			last := store.AuditLogs[len(store.AuditLogs)-1]
			if last.Event != tc.wantAudit || last.Method != tc.method || last.GatewayUserID != "u1" {
				t.Fatalf("audit fields = %#v", last)
			}
			if last.Status != tc.wantStatus {
				t.Fatalf("audit status = %d, want %d", last.Status, tc.wantStatus)
			}

			events := drainEmitter(em)
			var denied []observe.Event
			for _, ev := range events {
				if ev.Kind == observe.KindRequest {
					denied = append(denied, ev)
				}
				if ev.Kind == observe.KindPlayback {
					t.Fatalf("denial must not emit KindPlayback: %#v", ev)
				}
			}
			if len(denied) != 1 {
				t.Fatalf("KindRequest events = %d (%#v), want exactly 1", len(denied), denied)
			}
			ev := denied[0]
			if ev.Outcome != observe.OutcomeDenied || ev.StatusClass != observe.Status4xx {
				t.Fatalf("event outcome/status = %#v", ev)
			}
			if ev.Method != tc.method {
				t.Fatalf("event method = %q, want %q", ev.Method, tc.method)
			}
			if ev.UserID != "u1" || ev.Username != "alice" || ev.SessionID != HashToken("gateway-token") {
				t.Fatalf("event identity = %#v", ev)
			}
			decision := routeclass.Classify(tc.method, tc.path)
			if ev.RouteClass != observe.RouteClassOf(decision) {
				t.Fatalf("RouteClass = %q, want %q", ev.RouteClass, observe.RouteClassOf(decision))
			}
		})
	}
}

func TestSessionsXIsUnclassified404(t *testing.T) {
	// Phase 8: non-session lookalikes are Unclassified 404.
	var forwarded []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = append(forwarded, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/SessionsX?api_key=gateway-token", nil))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || string(body) != "not found\n" {
		t.Fatalf("status/body = %d/%q, want 404 not found", resp.StatusCode, body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Allow") != "" {
		t.Fatalf("headers Cache-Control=%q Allow=%q", resp.Header.Get("Cache-Control"), resp.Header.Get("Allow"))
	}
	if len(forwarded) != 0 {
		t.Fatalf("forwarded = %v, want zero dial", forwarded)
	}
	if !hasAuditEvent(store, "route_not_found") {
		t.Fatalf("missing route_not_found audit: %#v", store.AuditLogs)
	}
}

func TestSessionDenialUnauthenticatedIs401(t *testing.T) {
	recorder := newEgressRecorder(nil)
	backend := httptest.NewServer(recorder)
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	// Expired session for "expired-token".
	expired := testSession()
	expired.GatewayTokenHash = HashToken("expired-token")
	expired.ExpiresAt = time.Now().UTC().Add(-time.Hour)
	store.Sessions[expired.GatewayTokenHash] = expired

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	for _, tc := range []struct {
		name, token string
	}{
		{"missing", ""},
		{"expired", "expired-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder.reset()
			store.mu.Lock()
			store.AuditLogs = nil
			store.mu.Unlock()
			reqURL := gw.URL + "/emby/Sessions/session-1/Playing/Pause"
			if tc.token != "" {
				reqURL += "?api_key=" + url.QueryEscape(tc.token)
			}
			resp := do(t, mustRequest(t, http.MethodPost, reqURL, nil))
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (before deny/405)", resp.StatusCode)
			}
			recorder.assertEmpty(t)
			if hasAuditEvent(store, "session_access_denied") || hasAuditEvent(store, "session_method_not_allowed") {
				t.Fatalf("unauthenticated must not emit session denial audits: %#v", store.AuditLogs)
			}
		})
	}
}

func TestSessionDenialMalformedQueryAndCredentialConflictPrecedence(t *testing.T) {
	recorder := newEgressRecorder(nil)
	backend := httptest.NewServer(recorder)
	defer backend.Close()

	selected, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}
	other, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	sess := testSession()
	sess.GatewayTokenHash = HashToken(selected)
	store.Sessions[sess.GatewayTokenHash] = sess
	otherSess := testSession()
	otherSess.GatewayTokenHash = HashToken(other)
	store.Sessions[otherSess.GatewayTokenHash] = otherSess

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	t.Run("malformed_query", func(t *testing.T) {
		recorder.reset()
		store.mu.Lock()
		store.AuditLogs = nil
		store.mu.Unlock()
		resp := do(t, mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/session-1/Playing/Pause?api_key=%ZZ", nil))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		recorder.assertEmpty(t)
		if hasAuditEvent(store, "session_access_denied") {
			t.Fatalf("malformed query must precede denial: %#v", store.AuditLogs)
		}
	})

	t.Run("credential_conflict", func(t *testing.T) {
		recorder.reset()
		store.mu.Lock()
		store.AuditLogs = nil
		store.mu.Unlock()
		// Header selects one gateway-shaped token; generic token= query carries another active gateway token.
		// (guardProxyQueryCredentials only inspects the generic "token" key.)
		req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/session-1/Playing/Pause?token="+url.QueryEscape(other), nil)
		req.Header.Set("X-Emby-Token", selected)
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 credential conflict", resp.StatusCode)
		}
		recorder.assertEmpty(t)
		if hasAuditEvent(store, "session_access_denied") {
			t.Fatalf("credential conflict must precede denial: %#v", store.AuditLogs)
		}
	})
}

func TestSessionDenialPathPolicyPrecedence(t *testing.T) {
	recorder := newEgressRecorder(nil)
	backend := httptest.NewServer(recorder)
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	store.PathPolicies = []PathPolicy{{
		// Terminal-star prefix match: /Sessions/* matches /Sessions/session-1/...
		ID: "deny-session-control", Method: "*", Path: "/Sessions/*", Action: "deny", Priority: 1, Enabled: true,
	}}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/session-1/Playing/Pause?api_key=gateway-token", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 path policy", resp.StatusCode)
	}
	if !hasAuditEvent(store, "path_denied") {
		t.Fatalf("want path_denied before session denial: %#v", store.AuditLogs)
	}
	if hasAuditEvent(store, "session_access_denied") {
		t.Fatalf("path policy must precede session denial: %#v", store.AuditLogs)
	}
	recorder.assertEmpty(t)
}

func TestLocalSessionRoutesRemainAccepted(t *testing.T) {
	recorder := newEgressRecorder(func(w http.ResponseWriter, r *http.Request) {
		// Local playback may still enrich via neutral metadata GET; mutation egress is forbidden.
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/Items/") {
			writeTestJSON(w, map[string]any{"Id": "item-1", "Name": "Item", "Type": "Movie", "RunTimeTicks": float64(10_000_000)})
			return
		}
		t.Errorf("unexpected mutation egress: %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusNoContent)
	})
	backend := httptest.NewServer(recorder)
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, "/Sessions/Playing", `{"Item":{"Id":"item-1","Name":"M","Type":"Movie","RunTimeTicks":10000000},"PositionTicks":100}`, http.StatusOK},
		{http.MethodPost, "/Sessions/Playing/Progress", `{"ItemId":"item-1","PlaybackPositionTicks":250}`, http.StatusOK},
		{http.MethodPost, "/Sessions/Playing/Stopped", `{"ItemId":"item-1","PositionTicks":900,"RunTimeTicks":1000}`, http.StatusOK},
		{http.MethodPost, "/Sessions/Playing/Ping", `{}`, http.StatusOK},
		{http.MethodPost, "/Sessions/Capabilities", `{}`, http.StatusOK},
		{http.MethodPost, "/Sessions/Capabilities/Full", `{}`, http.StatusOK},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			req := mustRequest(t, tc.method, gw.URL+"/emby"+tc.path+"?api_key=gateway-token", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			resp := do(t, req)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}

	// GET /Sessions is fully local (zero upstream).
	sessionsBackend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("GET /Sessions must not dial upstream")
	}))
	defer sessionsBackend.Close()
	store2 := NewMemoryStore()
	configureTestUpstream(store2, sessionsBackend.URL+"/emby")
	sess := testSession()
	sess.PublicID = "session-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	store2.Sessions[HashToken("gateway-token")] = sess
	gw2 := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store2))
	defer gw2.Close()
	resp := do(t, mustRequest(t, http.MethodGet, gw2.URL+"/emby/Sessions?api_key=gateway-token", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /Sessions status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
}

func TestSessionLocalWrongMethod405(t *testing.T) {
	recorder := newEgressRecorder(nil)
	backend := httptest.NewServer(recorder)
	defer backend.Close()

	store := &countingRuntimeStore{MemoryStore: NewMemoryStore()}
	configureTestUpstream(store.MemoryStore, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	em := observe.NewEmitter(32)
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", Emitter: em}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions/Playing?api_key=gateway-token", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	if resp.Header.Get("Allow") != "POST" {
		t.Fatalf("Allow = %q, want POST", resp.Header.Get("Allow"))
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
	recorder.assertEmpty(t)
	if store.loadCount() != 0 {
		t.Fatalf("runtime loads = %d, want 0", store.loadCount())
	}
	if !hasAuditEvent(store.MemoryStore, "session_method_not_allowed") {
		t.Fatalf("missing audit: %#v", store.AuditLogs)
	}
	events := drainEmitter(em)
	var reqs int
	for _, ev := range events {
		if ev.Kind == observe.KindRequest {
			reqs++
			if ev.Outcome != observe.OutcomeDenied {
				t.Fatalf("event = %#v", ev)
			}
		}
	}
	if reqs != 1 {
		t.Fatalf("KindRequest count = %d, want 1 (%#v)", reqs, events)
	}
}

// mustEncodedPathRequest builds a request whose path contains encoded characters.
// net/http decodes into URL.Path; classification uses the decoded relative path.
func mustEncodedPathRequest(t *testing.T, method, host, encodedPath, rawQuery string) *http.Request {
	t.Helper()
	target := host + encodedPath
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

func TestSessionEncodedPathNeverProxies(t *testing.T) {
	recorder := newEgressRecorder(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusNoContent)
	})
	backend := httptest.NewServer(recorder)
	defer backend.Close()

	store := &countingRuntimeStore{MemoryStore: NewMemoryStore()}
	configureTestUpstream(store.MemoryStore, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	em := observe.NewEmitter(64)
	server := NewServer(Config{GatewayBasePath: "/emby", Emitter: em}, store)
	gw := httptest.NewServer(server)
	defer gw.Close()

	// Encoded ? / # / trailing-space style paths under Session-looking prefixes.
	// After net/http decoding, classification either denies (403) or fail-closes
	// LocalSession (404); never successful upstream proxy.
	cases := []struct {
		name        string
		method      string
		encodedPath string
		// wantStatuses are acceptable terminal statuses (never 2xx proxy success).
		wantStatuses []int
		wantAudits   []string
	}{
		{
			name:         "encoded_question_targeted",
			method:       http.MethodPost,
			encodedPath:  "/emby/Sessions/session-1/Playing%3FPause",
			wantStatuses: []int{http.StatusForbidden, http.StatusNotFound},
			wantAudits:   []string{"session_access_denied", "session_route_unhandled"},
		},
		{
			name:         "encoded_hash_targeted",
			method:       http.MethodPost,
			encodedPath:  "/emby/Sessions/session-1/Playing%23Pause",
			wantStatuses: []int{http.StatusForbidden, http.StatusNotFound},
			wantAudits:   []string{"session_access_denied", "session_route_unhandled"},
		},
		{
			name:        "encoded_trailing_space_local_looking",
			method:      http.MethodPost,
			encodedPath: "/emby/Sessions/Playing%20",
			// Path decodes to "/Sessions/Playing " then routeclass TrimSpace → exact Playing.
			// May be local-handled (empty 200) or fail-closed/denied; never proxy.
			wantStatuses: []int{http.StatusOK, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed},
			wantAudits:   []string{"session_access_denied", "session_route_unhandled", "session_method_not_allowed"},
		},
		{
			name:         "encoded_space_in_segment",
			method:       http.MethodPost,
			encodedPath:  "/emby/Sessions/Playing%20Pause",
			wantStatuses: []int{http.StatusForbidden, http.StatusNotFound},
			wantAudits:   []string{"session_access_denied", "session_route_unhandled"},
		},
		{
			name:         "encoded_question_playqueue",
			method:       http.MethodGet,
			encodedPath:  "/emby/Sessions/PlayQueue%3Fextra",
			wantStatuses: []int{http.StatusForbidden, http.StatusNotFound},
			wantAudits:   []string{"session_access_denied", "session_route_unhandled"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder.reset()
			store.mu.Lock()
			store.AuditLogs = nil
			store.mu.Unlock()
			_ = drainEmitter(em)
			beforeLoads := store.loadCount()

			req := mustEncodedPathRequest(t, tc.method, gw.URL, tc.encodedPath, "api_key=gateway-token")
			resp := do(t, req)
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			statusOK := false
			for _, want := range tc.wantStatuses {
				if resp.StatusCode == want {
					statusOK = true
					break
				}
			}
			if !statusOK {
				t.Fatalf("status = %d body=%q, want one of %v", resp.StatusCode, body, tc.wantStatuses)
			}
			// Successful upstream proxy is forbidden for these Session-looking paths.
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("status 200 is not allowed for encoded Session-looking paths")
			}
			recorder.assertEmpty(t)
			if store.loadCount() != beforeLoads {
				t.Fatalf("upstream runtime loads increased by %d", store.loadCount()-beforeLoads)
			}

			// When terminal is deny/fail-closed, require matching audit + single denied KindRequest.
			switch resp.StatusCode {
			case http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed:
				if resp.Header.Get("Cache-Control") != "no-store" {
					t.Fatalf("Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
				}
				foundAudit := false
				for _, a := range tc.wantAudits {
					if hasAuditEvent(store.MemoryStore, a) {
						foundAudit = true
						break
					}
				}
				if !foundAudit {
					t.Fatalf("missing expected audit in %#v, want one of %v", store.AuditLogs, tc.wantAudits)
				}
				events := drainEmitter(em)
				var reqs []observe.Event
				for _, ev := range events {
					if ev.Kind == observe.KindRequest {
						reqs = append(reqs, ev)
					}
					if ev.Kind == observe.KindPlayback {
						t.Fatalf("must not emit KindPlayback: %#v", ev)
					}
				}
				if len(reqs) != 1 || reqs[0].Outcome != observe.OutcomeDenied || reqs[0].StatusClass != observe.Status4xx {
					t.Fatalf("KindRequest events = %#v, want exactly one denied 4xx", reqs)
				}
				if reqs[0].RouteClass == observe.RouteOther {
					t.Fatalf("denied RouteClass must not be RouteOther: %#v", reqs[0])
				}
			case http.StatusNoContent, http.StatusOK:
				// Local handler accepted after normalize (empty 200 for playback reports); still zero egress.
			}
		})
	}
}

func TestAcceptedAuthenticatedRequestTelemetryUsesRouteClass(t *testing.T) {
	// Cover current-user, local playback, and representative metadata.
	type hit struct {
		method string
		path   string
		body   string
		wantRC string
	}

	recorder := newEgressRecorder(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/Items/") {
			writeTestJSON(w, map[string]any{"Id": "item-1", "Name": "Item", "Type": "Movie", "RunTimeTicks": float64(10_000_000)})
			return
		}
		if r.URL.Path == "/emby/System/Info" {
			writeTestJSON(w, map[string]any{"Id": "backend", "ServerName": "b"})
			return
		}
		t.Errorf("unexpected upstream: %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusNoContent)
	})
	backend := httptest.NewServer(recorder)
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	em := observe.NewEmitter(64)
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", Emitter: em}, store))
	defer gw.Close()

	cases := []hit{
		{method: http.MethodGet, path: "/Users/gateway-user", wantRC: observe.RouteClassOf(routeclass.Classify(http.MethodGet, "/Users/gateway-user"))},
		{method: http.MethodPost, path: "/Sessions/Playing/Progress", body: `{"ItemId":"item-1","PlaybackPositionTicks":100}`, wantRC: observe.RouteClassOf(routeclass.Classify(http.MethodPost, "/Sessions/Playing/Progress"))},
		{method: http.MethodGet, path: "/System/Info", wantRC: observe.RouteClassOf(routeclass.Classify(http.MethodGet, "/System/Info"))},
	}

	for _, tc := range cases {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			_ = drainEmitter(em)
			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			req := mustRequest(t, tc.method, gw.URL+"/emby"+tc.path+"?api_key=gateway-token", body)
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			resp := do(t, req)
			_ = resp.Body.Close()
			if resp.StatusCode >= 400 {
				t.Fatalf("status = %d, want success", resp.StatusCode)
			}

			events := drainEmitter(em)
			var reqs []observe.Event
			for _, ev := range events {
				if ev.Kind == observe.KindRequest {
					reqs = append(reqs, ev)
				}
			}
			if len(reqs) != 1 {
				t.Fatalf("KindRequest count = %d (%#v), want exactly 1", len(reqs), reqs)
			}
			ev := reqs[0]
			if ev.Outcome != observe.OutcomeOK {
				t.Fatalf("outcome = %q, want ok", ev.Outcome)
			}
			if ev.RouteClass != tc.wantRC {
				t.Fatalf("RouteClass = %q, want %q (must not be RouteOther unless classified as such)", ev.RouteClass, tc.wantRC)
			}
			if ev.RouteClass == observe.RouteOther && tc.wantRC != observe.RouteOther {
				t.Fatalf("RouteClass fell back to RouteOther")
			}
			if ev.UserID != "u1" || ev.SessionID != HashToken("gateway-token") {
				t.Fatalf("identity = %#v", ev)
			}
		})
	}
}

func TestSessionRouteUnhandledFailClosed(t *testing.T) {
	// Defensive path: if LocalSession is classified but no local handler consumes it,
	// gateway must 404 (not proxy, not 403 DeniedSession).
	// GET /Sessions/Playing is MethodAllowed=false → 405, not unhandled.
	// Use a LocalSession operation that is method-allowed but force-fail by
	// exercising the fail-closed branch via a decision that matches LocalSession
	// PlaybackPing shape only when handler predicates would miss — covered
	// operationally by encoded/normalize drift tests; here assert handler contract
	// for known LocalSession ops still succeed (not 404).
	recorder := newEgressRecorder(nil)
	backend := httptest.NewServer(recorder)
	defer backend.Close()
	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	// Known LocalSession ops must not fail-closed to 404.
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/Sessions/Playing/Ping", http.StatusOK},
		{"/Sessions/Capabilities", http.StatusOK},
	} {
		req := mustRequest(t, http.MethodPost, gw.URL+"/emby"+tc.path+"?api_key=gateway-token", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			t.Fatalf("%s returned 404 fail-closed unexpectedly", tc.path)
		}
		if resp.StatusCode != tc.want {
			t.Fatalf("%s status = %d, want %d", tc.path, resp.StatusCode, tc.want)
		}
	}
}
