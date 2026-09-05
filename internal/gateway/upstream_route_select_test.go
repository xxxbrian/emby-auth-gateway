package gateway

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testRuntimeWithEndpoints builds a managed runtime with a default endpoint
// and an extra enabled endpoint.
func testRuntimeWithEndpoints(defaultURL, extraURL string) *UpstreamRuntime {
	now := time.Now().UTC()
	return &UpstreamRuntime{
		Source: UpstreamSource{
			ID: "source", Key: "default", ServerID: "server",
			BackendUsername: "backend", BackendPassword: "password",
			BackendUserID: "user", BackendToken: "token",
			AuthGenerationID: "generation",
			ClientIdentity:   BackendClientIdentity{UserAgent: "agent", Client: "client", Device: "device", DeviceID: "device-id", Version: "1"},
			TokenUpdatedAt:   &now, LastLoginAt: &now,
		},
		Endpoints: UpstreamEndpoints{
			{ID: "ep-default", SourceID: "source", Key: "primary", BaseURL: defaultURL, Enabled: true, Default: true},
			{ID: "ep-ws", SourceID: "source", Key: "cf", BaseURL: extraURL, Enabled: true},
		},
	}
}

func TestSelectUpstreamSnapshotRoutesWebSocketToEndpointKey(t *testing.T) {
	store := NewMemoryStore()
	runtime := testRuntimeWithEndpoints("https://default.example", "https://cf.example")
	store.UpstreamSources["source"] = runtime.Source
	store.UpstreamEndpoints["primary"] = runtime.Endpoints[0]
	store.UpstreamEndpoints["cf"] = runtime.Endpoints[1]
	store.RouteRules = []RouteRule{
		{ID: "r1", Method: "", Path: "/socket", Transport: "websocket", Target: "cf", Priority: 10, Enabled: true},
	}
	server := NewServer(Config{}, store)

	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/socket", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")

	snapshot, err := server.selectUpstreamSnapshot(context.Background(), runtime, req, "/socket")
	if err != nil {
		t.Fatalf("select snapshot: %v", err)
	}
	if snapshot.baseURL != "https://cf.example" {
		t.Fatalf("websocket snapshot base = %q, want cf", snapshot.baseURL)
	}
	if snapshot.token != "token" || snapshot.userID != "user" || snapshot.serverID != "server" {
		t.Fatalf("shared auth dropped: %#v", snapshot)
	}
	if snapshot.endpointKey != "cf" {
		t.Fatalf("endpoint key = %q, want cf", snapshot.endpointKey)
	}
}

func TestSelectUpstreamSnapshotDefaultsWhenNoRuleMatches(t *testing.T) {
	store := NewMemoryStore()
	runtime := testRuntimeWithEndpoints("https://default.example", "https://cf.example")
	store.UpstreamSources["source"] = runtime.Source
	store.UpstreamEndpoints["primary"] = runtime.Endpoints[0]
	store.UpstreamEndpoints["cf"] = runtime.Endpoints[1]
	store.RouteRules = []RouteRule{
		{ID: "r1", Method: "GET", Path: "/socket", Transport: "websocket", Target: "cf", Priority: 10, Enabled: true},
	}
	server := NewServer(Config{}, store)

	// Ordinary request to an unrelated path -> default.
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items", nil)
	snapshot, err := server.selectUpstreamSnapshot(context.Background(), runtime, req, "/Items")
	if err != nil {
		t.Fatalf("select snapshot: %v", err)
	}
	if snapshot.baseURL != "https://default.example" {
		t.Fatalf("default snapshot base = %q", snapshot.baseURL)
	}
	if snapshot.endpointKey != "primary" {
		t.Fatalf("default endpoint key = %q", snapshot.endpointKey)
	}
}

func TestSelectUpstreamSnapshotRuleTargetMissingFallsBackToDefault(t *testing.T) {
	store := NewMemoryStore()
	runtime := testRuntimeWithEndpoints("https://default.example", "https://cf.example")
	store.UpstreamSources["source"] = runtime.Source
	store.UpstreamEndpoints["primary"] = runtime.Endpoints[0]
	// Drop the cf endpoint so the rule target is unavailable.
	store.RouteRules = []RouteRule{
		{ID: "r1", Method: "", Path: "/socket", Transport: "websocket", Target: "missing", Priority: 10, Enabled: true},
	}
	server := NewServer(Config{}, store)

	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/socket", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	snapshot, err := server.selectUpstreamSnapshot(context.Background(), runtime, req, "/socket")
	if err != nil {
		t.Fatalf("select snapshot: %v", err)
	}
	if snapshot.baseURL != "https://default.example" || snapshot.endpointKey != "primary" {
		t.Fatalf("fallback = %q/%q, want default", snapshot.baseURL, snapshot.endpointKey)
	}
}

// TestGatewayWebSocketRoutesToRuleEndpointEndToEnd verifies that a WebSocket
// Upgrade matching a route rule is proxied to the rule's endpoint (not the
// default), while ordinary HTTP stays on the default endpoint.
func TestGatewayWebSocketRoutesToRuleEndpointEndToEnd(t *testing.T) {
	const (
		backendToken    = "backend-token-secret"
		backendUserID   = "backend-user-id"
		syntheticUserID = "gateway-user-id"
	)
	var defaultWSHits int32

	// Default backend: REST + auth only. It must never see the WS upgrade.
	defaultBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Users/AuthenticateByName":
			writeTestJSON(w, map[string]any{"AccessToken": backendToken, "ServerId": "backend-server-id", "User": map[string]any{"Id": backendUserID, "Name": "shared"}})
		case "/emby/System/Info":
			w.WriteHeader(http.StatusOK)
		case "/emby/embywebsocket":
			atomic.AddInt32(&defaultWSHits, 1)
			t.Fatalf("default backend received websocket upgrade")
		default:
			t.Fatalf("unexpected default backend request %s %s", r.Method, r.URL.String())
		}
	}))
	defer defaultBackend.Close()

	// CDN backend: WebSocket endpoint only.
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/embywebsocket" {
			t.Fatalf("unexpected cdn request %s %s", r.Method, r.URL.String())
		}
		if r.URL.Query().Get("api_key") != backendToken || r.Header.Get("X-Emby-Token") != backendToken {
			t.Fatalf("cdn upgrade missing token: query=%q header=%q", r.URL.RawQuery, r.Header.Get("X-Emby-Token"))
		}
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			t.Fatalf("expected websocket upgrade on cdn, got %q", r.Header.Get("Upgrade"))
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("response writer does not support hijacking")
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack cdn: %v", err)
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\ncdn-upgrade-ok")
		_ = rw.Flush()
	}))
	defer cdn.Close()

	store := NewMemoryStore()
	store.Users["u1"] = MemoryUser{GatewayUser: GatewayUser{ID: "u1", Username: "alice", SyntheticUserID: syntheticUserID, Enabled: true}, Password: "alice-pass"}
	configureTestUpstream(store, defaultBackend.URL+"/emby")
	source := store.UpstreamSources["source"]
	source.BackendToken = backendToken
	source.BackendUserID = backendUserID
	source.ServerID = "backend-server-id"
	store.UpstreamSources["source"] = source
	store.UpstreamEndpoints["cf"] = UpstreamEndpoint{ID: "cf", SourceID: "source", Key: "cf", BaseURL: cdn.URL + "/emby", Enabled: true, Default: false}
	store.RouteRules = []RouteRule{
		{ID: "r-ws", Method: "", Path: "/embywebsocket", Transport: "websocket", Target: "cf", Priority: 100, Enabled: true},
	}

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	// Login (goes to default backend).
	loginReq := mustRequest(t, http.MethodPost, gw.URL+"/emby/Users/AuthenticateByName", strings.NewReader(`{"Username":"alice","Pw":"alice-pass"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp := do(t, loginReq)
	defer loginResp.Body.Close()
	var login map[string]any
	decodeJSON(t, loginResp.Body, &login)
	gatewayToken, _ := login["AccessToken"].(string)
	if gatewayToken == "" {
		t.Fatal("missing gateway token")
	}

	// Ordinary REST request stays on default backend.
	infoReq := mustRequest(t, http.MethodGet, gw.URL+"/emby/System/Info", nil)
	infoReq.Header.Set("X-Emby-Token", gatewayToken)
	infoResp := do(t, infoReq)
	_ = infoResp.Body.Close()

	// WebSocket Upgrade goes to the cf endpoint.
	gwURL, err := url.Parse(gw.URL)
	if err != nil {
		t.Fatalf("parse gateway url: %v", err)
	}
	conn, err := net.Dial("tcp", gwURL.Host)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("GET /emby/embywebsocket?api_key=" + gatewayToken + "&deviceId=web-device HTTP/1.1\r\n" +
		"Host: " + gwURL.Host + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"))

	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read upgrade status: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("expected 101 upgrade via cdn, got %q", status)
	}
	if got := atomic.LoadInt32(&defaultWSHits); got != 0 {
		t.Fatalf("default backend saw %d websocket upgrades, want 0", got)
	}
}

// TestRefreshAfterUnauthorizedOnRoutedEndpointDoesNotRotateWhenDefaultHealthy
// guards H1: a 401 observed on a routed (non-default) endpoint must not rotate
// the shared token when the default endpoint still accepts it.
func TestRefreshAfterUnauthorizedDoesNotRotateWhenOnlyRoutedEndpoint401s(t *testing.T) {
	var defaultInfoHits, cdnInfoHits int
	var logins int

	defaultBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Users/AuthenticateByName":
			logins++
			writeTestJSON(w, map[string]any{"AccessToken": "fresh-token", "ServerId": "backend-server-id", "User": map[string]any{"Id": "backend-user", "Name": "shared"}})
		case "/emby/System/Info":
			defaultInfoHits++
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected default request %s %s", r.Method, r.URL.String())
		}
	}))
	defer defaultBackend.Close()

	// CDN endpoint: WS-only; REST /System/Info returns 401 (CDN gating).
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/System/Info":
			cdnInfoHits++
			w.WriteHeader(http.StatusUnauthorized)
		case "/emby/embywebsocket":
			w.WriteHeader(http.StatusSwitchingProtocols)
		default:
			t.Fatalf("unexpected cdn request %s %s", r.Method, r.URL.String())
		}
	}))
	defer cdn.Close()

	store := NewMemoryStore()
	store.Users["u1"] = MemoryUser{GatewayUser: GatewayUser{ID: "u1", Username: "alice", SyntheticUserID: "g1", Enabled: true}, Password: "alice-pass"}
	configureTestUpstream(store, defaultBackend.URL+"/emby")
	source := store.UpstreamSources["source"]
	source.BackendToken = "original-token"
	source.BackendUserID = "backend-user"
	source.ServerID = "backend-server-id"
	source.AuthGenerationID = "gen-1"
	store.UpstreamSources["source"] = source
	store.UpstreamEndpoints["cf"] = UpstreamEndpoint{ID: "cf", SourceID: "source", Key: "cf", BaseURL: cdn.URL + "/emby", Enabled: true, Default: false}
	// Wire the source client identity token used by rewriteRequestHeaders.
	store.RouteRules = []RouteRule{
		{ID: "r-ws", Method: "", Path: "/embywebsocket", Transport: "websocket", Target: "cf", Priority: 100, Enabled: true},
	}

	server := NewServer(Config{GatewayBasePath: "/emby"}, store)
	// Build a snapshot for the cf endpoint and simulate the WS 401 flow.
	runtime, err := store.LoadDefaultUpstreamRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cfSnapshot, err := upstreamRequestSnapshotFromRuntimeEndpoint(runtime, "cf")
	if err != nil {
		t.Fatal(err)
	}
	refreshed, confirmed, err := server.refreshAfterUnauthorized(context.Background(), cfSnapshot)
	if err != nil {
		t.Fatalf("refreshAfterUnauthorized: %v", err)
	}
	if confirmed {
		t.Fatalf("token rotated even though default endpoint is healthy (logins=%d)", logins)
	}
	if refreshed.token != "original-token" {
		t.Fatalf("token changed without rotation: %q", refreshed.token)
	}
	if defaultInfoHits == 0 {
		t.Fatalf("default endpoint was never probed for authoritative confirmation")
	}
	if logins != 0 {
		t.Fatalf("unexpected login attempt: %d", logins)
	}
}

// TestRefreshAfterUnauthorizedRotatesWhenDefaultEndpoint401s verifies the
// legacy behavior still works: when the authoritative default endpoint itself
// returns 401 for the shared token, rotation proceeds.
func TestRefreshAfterUnauthorizedRotatesWhenDefaultEndpoint401s(t *testing.T) {
	var logins int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Users/AuthenticateByName":
			logins++
			writeTestJSON(w, map[string]any{"AccessToken": "fresh-token", "ServerId": "backend-server-id", "User": map[string]any{"Id": "backend-user", "Name": "shared"}})
		case "/emby/System/Info":
			w.WriteHeader(http.StatusUnauthorized)
		case "/emby/Sessions/Logout":
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	source := store.UpstreamSources["source"]
	source.BackendToken = "stale-token"
	source.BackendUserID = "backend-user"
	source.ServerID = "backend-server-id"
	store.UpstreamSources["source"] = source

	server := NewServer(Config{GatewayBasePath: "/emby"}, store)
	runtime, err := store.LoadDefaultUpstreamRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := upstreamRequestSnapshotFromRuntime(runtime)
	if err != nil {
		t.Fatal(err)
	}
	refreshed, confirmed, err := server.refreshAfterUnauthorized(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("refreshAfterUnauthorized: %v", err)
	}
	if !confirmed {
		t.Fatal("expected confirmed rotation for genuinely expired token")
	}
	if refreshed.token != "fresh-token" {
		t.Fatalf("token = %q, want fresh-token", refreshed.token)
	}
	if logins != 1 {
		t.Fatalf("logins = %d, want 1", logins)
	}
}
