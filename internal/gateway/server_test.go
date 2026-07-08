package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

var testHTTPClient = &http.Client{Timeout: 5 * time.Second}

func TestGatewayMVPTokenMappingAndRewriting(t *testing.T) {
	const (
		backendToken    = "backend-token-secret"
		backendUserID   = "backend-user-id"
		backendServerID = "backend-server-id"
		syntheticUserID = "gateway-user-id"
	)

	var backendURL string
	var sawControlledBackendLogin bool
	var sawBackendAuthUserAgent bool
	var sawBackendTokenInRequest bool
	var sawBackendUserInPath bool
	var sawBackendTokenFromAPIKey bool

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/emby/Users/AuthenticateByName":
			if r.UserAgent() == "Emby for Android/3.4.20" {
				sawBackendAuthUserAgent = true
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend auth body: %v", err)
			}
			if body["Username"] == "shared" && body["Pw"] == "backend-pass" {
				sawControlledBackendLogin = true
			}
			writeTestJSON(w, map[string]any{
				"AccessToken": backendToken,
				"ServerId":    backendServerID,
				"User": map[string]any{
					"Id":       backendUserID,
					"Name":     "shared",
					"ServerId": backendServerID,
				},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/emby/System/Info":
			if r.Header.Get("X-Emby-Token") == backendToken {
				sawBackendTokenInRequest = true
			}
			writeTestJSON(w, map[string]any{
				"Id":              backendServerID,
				"ServerName":      "Real Emby",
				"LocalAddress":    "http://backend-lan:8096/emby",
				"WanAddress":      "http://backend-wan:8096/emby",
				"RemoteAddresses": []string{"http://backend-remote:8096/emby"},
				"LocalAddresses":  []string{"http://backend-local:8096/emby"},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/emby/Users/"+backendUserID+"/Items":
			sawBackendUserInPath = true
			if r.URL.Query().Get("api_key") == backendToken {
				sawBackendTokenFromAPIKey = true
			}
			if r.URL.Query().Get("UserId") != backendUserID {
				t.Fatalf("expected backend UserId query, got %q", r.URL.Query().Get("UserId"))
			}
			writeTestJSON(w, map[string]any{
				"Items": []any{
					map[string]any{
						"Id":              "item-1",
						"ServerId":        backendServerID,
						"UserId":          backendUserID,
						"DirectStreamUrl": backendURL + "/emby/Videos/item-1/stream?api_key=" + backendToken,
					},
				},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/emby/Videos/item-1/master.m3u8":
			if r.URL.Query().Get("api_key") != backendToken {
				t.Fatalf("expected backend token in m3u8 query, got %q", r.URL.Query().Get("api_key"))
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte("#EXTM3U\n" + backendURL + "/emby/Videos/item-1/hls/seg0.ts?api_key=" + backendToken + "\n"))

		case r.Method == http.MethodGet && r.URL.Path == "/emby/Videos/item-1/stream":
			if r.URL.Query().Get("api_key") != backendToken {
				t.Fatalf("expected backend token in stream query, got %q", r.URL.Query().Get("api_key"))
			}
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("video-bytes"))

		case r.Method == http.MethodPost && r.URL.Path == "/emby/Sessions/Logout":
			if r.Header.Get("X-Emby-Token") != backendToken {
				t.Fatalf("expected backend logout token, got %q", r.Header.Get("X-Emby-Token"))
			}
			w.WriteHeader(http.StatusOK)

		default:
			t.Fatalf("unexpected backend request %s %s", r.Method, r.URL.String())
		}
	}))
	defer backend.Close()
	backendURL = backend.URL

	store := NewMemoryStore()
	store.Users["u1"] = MemoryUser{
		GatewayUser: GatewayUser{ID: "u1", Username: "alice", SyntheticUserID: syntheticUserID, Enabled: true},
		Password:    "alice-pass",
	}
	store.Mappings["u1"] = UserMapping{
		ID:               "m1",
		GatewayUserID:    "u1",
		BackendAccountID: "b1",
		Enabled:          true,
		BackendAccount: BackendAccount{
			ID:       "b1",
			ServerID: "s1",
			BaseURL:  backend.URL + "/emby",
			Username: "shared",
			Password: "backend-pass",
			Enabled:  true,
		},
	}

	gw := httptest.NewServer(NewServer(Config{
		PublicBaseURL:   "https://media.example.com",
		GatewayBasePath: "/emby",
		GatewayServerID: "gateway-server-id",
	}, store))
	defer gw.Close()

	loginBody := `{"Username":"alice","Pw":"alice-pass"}`
	loginReq, _ := http.NewRequest(http.MethodPost, gw.URL+"/emby/Users/AuthenticateByName", strings.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginReq.Header.Set("User-Agent", "Emby for Android/3.4.20")
	loginReq.Header.Set("X-Emby-Authorization", `Emby Client="Emby for Android", Device="Android Phone", DeviceId="android-client-dev-1", Version="3.4.20"`)
	loginResp := do(t, loginReq)
	defer loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(loginResp.Body)
		t.Fatalf("login status %d: %s", loginResp.StatusCode, string(body))
	}
	var login map[string]any
	decodeJSON(t, loginResp.Body, &login)
	gatewayToken, _ := login["AccessToken"].(string)
	if gatewayToken == "" || gatewayToken == backendToken {
		t.Fatalf("expected gateway token, got %q", gatewayToken)
	}
	if !sawControlledBackendLogin {
		t.Fatal("backend did not receive controlled backend account credentials")
	}
	if !sawBackendAuthUserAgent {
		t.Fatal("backend authentication did not receive client user agent")
	}
	if strings.Contains(mustJSON(t, login), backendToken) || strings.Contains(mustJSON(t, login), backendUserID) {
		t.Fatalf("login leaked backend token or user id: %s", mustJSON(t, login))
	}

	systemReq, _ := http.NewRequest(http.MethodGet, gw.URL+"/emby/System/Info", nil)
	systemReq.Header.Set("X-Emby-Token", gatewayToken)
	systemResp := do(t, systemReq)
	defer systemResp.Body.Close()
	if systemResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(systemResp.Body)
		t.Fatalf("system status %d: %s", systemResp.StatusCode, string(body))
	}
	var system map[string]any
	decodeJSON(t, systemResp.Body, &system)
	systemJSON := mustJSON(t, system)
	if !sawBackendTokenInRequest {
		t.Fatal("backend did not receive mapped token")
	}
	if strings.Contains(systemJSON, backend.URL) || strings.Contains(systemJSON, backendServerID) {
		t.Fatalf("system info leaked backend details: %s", systemJSON)
	}
	if !strings.Contains(systemJSON, "https://media.example.com/emby") {
		t.Fatalf("system info did not include gateway url: %s", systemJSON)
	}
	for _, field := range []string{"LocalAddress", "WanAddress"} {
		if system[field] != "https://media.example.com/emby" {
			t.Fatalf("%s was not rewritten to gateway address: %#v", field, system[field])
		}
	}
	for _, field := range []string{"RemoteAddresses", "LocalAddresses"} {
		values, ok := system[field].([]any)
		if !ok || len(values) != 1 || values[0] != "https://media.example.com/emby" {
			t.Fatalf("%s was not rewritten to gateway address: %#v", field, system[field])
		}
	}

	itemsURL := gw.URL + "/emby/Users/" + syntheticUserID + "/Items?api_key=" + gatewayToken + "&UserId=" + syntheticUserID
	itemsResp := do(t, mustRequest(t, http.MethodGet, itemsURL, nil))
	defer itemsResp.Body.Close()
	itemsBody, _ := io.ReadAll(itemsResp.Body)
	itemsText := string(itemsBody)
	if !sawBackendUserInPath || !sawBackendTokenFromAPIKey {
		t.Fatal("backend did not receive mapped user id path or api_key token")
	}
	if strings.Contains(itemsText, backend.URL) || strings.Contains(itemsText, backendToken) || strings.Contains(itemsText, backendUserID) {
		t.Fatalf("items response leaked backend details: %s", itemsText)
	}
	if !strings.Contains(itemsText, "https://media.example.com/emby/Videos/item-1/stream?api_key="+gatewayToken) {
		t.Fatalf("items response did not rewrite stream url: %s", itemsText)
	}

	m3u8URL := gw.URL + "/emby/Videos/item-1/master.m3u8?api_key=" + gatewayToken
	m3u8Resp := do(t, mustRequest(t, http.MethodGet, m3u8URL, nil))
	defer m3u8Resp.Body.Close()
	m3u8Body, _ := io.ReadAll(m3u8Resp.Body)
	m3u8 := string(m3u8Body)
	if strings.Contains(m3u8, backend.URL) || strings.Contains(m3u8, backendToken) {
		t.Fatalf("m3u8 leaked backend details: %s", m3u8)
	}
	if !strings.Contains(m3u8, "https://media.example.com/emby/Videos/item-1/hls/seg0.ts?api_key="+gatewayToken) {
		t.Fatalf("m3u8 did not rewrite segment url: %s", m3u8)
	}

	streamURL := gw.URL + "/emby/Videos/item-1/stream?api_key=" + gatewayToken
	streamResp := do(t, mustRequest(t, http.MethodGet, streamURL, nil))
	defer streamResp.Body.Close()
	streamBody, _ := io.ReadAll(streamResp.Body)
	if string(streamBody) != "video-bytes" {
		t.Fatalf("unexpected stream body %q", string(streamBody))
	}

	logoutReq := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Logout", nil)
	logoutReq.Header.Set("X-Emby-Token", gatewayToken)
	logoutResp := do(t, logoutReq)
	_ = logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusOK {
		t.Fatalf("logout status %d", logoutResp.StatusCode)
	}

	postLogoutResp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/System/Info?api_key="+gatewayToken, nil))
	_ = postLogoutResp.Body.Close()
	if postLogoutResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected revoked token to be unauthorized, got %d", postLogoutResp.StatusCode)
	}
}

func TestGatewayWebSocketUpgradeProxy(t *testing.T) {
	const (
		backendToken    = "backend-token-secret"
		backendUserID   = "backend-user-id"
		syntheticUserID = "gateway-user-id"
	)
	var sawBackendToken bool

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Users/AuthenticateByName":
			writeTestJSON(w, map[string]any{
				"AccessToken": backendToken,
				"ServerId":    "backend-server-id",
				"User": map[string]any{
					"Id":   backendUserID,
					"Name": "shared",
				},
			})
		case "/emby/socket":
			if r.URL.Query().Get("api_key") == backendToken && r.Header.Get("X-Emby-Token") == backendToken {
				sawBackendToken = true
			}
			if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				t.Fatalf("expected websocket upgrade, got %q", r.Header.Get("Upgrade"))
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("response writer does not support hijacking")
			}
			conn, rw, err := hj.Hijack()
			if err != nil {
				t.Fatalf("hijack backend: %v", err)
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\nbackend-upgrade-ok")
			_ = rw.Flush()
		default:
			t.Fatalf("unexpected backend request %s %s", r.Method, r.URL.String())
		}
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Users["u1"] = MemoryUser{
		GatewayUser: GatewayUser{ID: "u1", Username: "alice", SyntheticUserID: syntheticUserID, Enabled: true},
		Password:    "alice-pass",
	}
	store.Mappings["u1"] = UserMapping{
		ID:               "m1",
		GatewayUserID:    "u1",
		BackendAccountID: "b1",
		Enabled:          true,
		BackendAccount: BackendAccount{
			ID:       "b1",
			BaseURL:  backend.URL + "/emby",
			Username: "shared",
			Password: "backend-pass",
			Enabled:  true,
		},
	}

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

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

	gwURL, err := url.Parse(gw.URL)
	if err != nil {
		t.Fatalf("parse gateway url: %v", err)
	}
	conn, err := net.Dial("tcp", gwURL.Host)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("GET /emby/socket?api_key=" + gatewayToken + " HTTP/1.1\r\n" +
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
		t.Fatalf("expected 101 upgrade, got %q", status)
	}
	if !sawBackendToken {
		t.Fatal("backend did not receive mapped token on websocket upgrade")
	}
}

func TestAnonymousPublicInfoAndPing(t *testing.T) {
	gw := httptest.NewServer(NewServer(Config{
		PublicBaseURL:   "https://media.example.com",
		GatewayBasePath: "/emby",
		GatewayServerID: "gateway-server-id",
	}, NewMemoryStore()))
	defer gw.Close()

	infoResp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/System/Info/Public", nil))
	defer infoResp.Body.Close()
	if infoResp.StatusCode != http.StatusOK {
		t.Fatalf("public info status %d", infoResp.StatusCode)
	}
	var info map[string]any
	decodeJSON(t, infoResp.Body, &info)
	body := mustJSON(t, info)
	if strings.Contains(body, "backend") || info["Id"] != "gateway-server-id" || info["ServerId"] != "gateway-server-id" {
		t.Fatalf("public info leaked backend details or missed gateway id: %s", body)
	}
	if info["LocalAddress"] != "https://media.example.com/emby" || info["WanAddress"] != "https://media.example.com/emby" {
		t.Fatalf("public info did not use gateway addresses: %s", body)
	}

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		pingResp := do(t, mustRequest(t, method, gw.URL+"/emby/System/Ping", nil))
		_ = pingResp.Body.Close()
		if pingResp.StatusCode != http.StatusOK {
			t.Fatalf("%s ping status %d", method, pingResp.StatusCode)
		}
	}
}

func TestAuthenticateByNameAcceptsFormBody(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/emby/Users/AuthenticateByName" {
			t.Fatalf("unexpected backend request %s %s", r.Method, r.URL.String())
		}
		writeTestJSON(w, map[string]any{
			"AccessToken": "backend-token",
			"ServerId":    "backend-server",
			"User": map[string]any{
				"Id":   "backend-user",
				"Name": "shared",
			},
		})
	}))
	defer backend.Close()

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, testStore(backend.URL+"/emby")))
	defer gw.Close()

	form := url.Values{}
	form.Set("Username", "alice")
	form.Set("Password", "alice-pass")
	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Users/AuthenticateByName", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("form login status %d: %s", resp.StatusCode, string(body))
	}
}

func TestAuthenticateByNameAcceptsJSONPasswordField(t *testing.T) {
	backend := testAuthBackend(t)
	defer backend.Close()

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, testStore(backend.URL+"/emby")))
	defer gw.Close()

	req := mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Password":"alice-pass"}`)
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("json Password login status %d: %s", resp.StatusCode, string(body))
	}
}

func TestLoginFailureRateLimitAndMappingStatus(t *testing.T) {
	store := testStore("http://127.0.0.1/emby")
	store.Mappings["u1"] = UserMapping{Enabled: false}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	validBody := `{"Username":"alice","Pw":"alice-pass"}`
	resp := do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", validBody))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled mapping status = %d, want 401", resp.StatusCode)
	}

	for i := 0; i < loginFailureLimit-1; i++ {
		resp := do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Pw":"bad"}`))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("bad login %d status = %d, want 401", i, resp.StatusCode)
		}
	}
	resp = do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", validBody))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("rate limited login status = %d, want 401", resp.StatusCode)
	}
}

func TestSuccessfulLoginClearsFailureCount(t *testing.T) {
	backend := testAuthBackend(t)
	defer backend.Close()

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, testStore(backend.URL+"/emby")))
	defer gw.Close()

	loginURL := gw.URL + "/emby/Users/AuthenticateByName"
	for i := 0; i < loginFailureLimit-1; i++ {
		resp := do(t, mustJSONLoginRequest(t, loginURL, `{"Username":"alice","Pw":"bad"}`))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("bad login %d status = %d, want 401", i, resp.StatusCode)
		}
	}

	resp := do(t, mustJSONLoginRequest(t, loginURL, `{"Username":"alice","Pw":"alice-pass"}`))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("successful login status = %d, want 200", resp.StatusCode)
	}

	resp = do(t, mustJSONLoginRequest(t, loginURL, `{"Username":"alice","Pw":"bad"}`))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-success bad login status = %d, want 401", resp.StatusCode)
	}

	resp = do(t, mustJSONLoginRequest(t, loginURL, `{"Username":"alice","Pw":"alice-pass"}`))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-clear successful login status = %d, want 200", resp.StatusCode)
	}
}

func TestAuditLogsForAuthAndLogout(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Users/AuthenticateByName":
			writeTestJSON(w, map[string]any{
				"AccessToken": "backend-token",
				"ServerId":    "backend-server",
				"User": map[string]any{
					"Id":   "backend-user",
					"Name": "shared",
				},
			})
		case "/emby/Sessions/Logout":
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected backend request %s %s", r.Method, r.URL.String())
		}
	}))
	defer backend.Close()

	store := testStore(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	badResp := do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Pw":"bad"}`))
	_ = badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad login status = %d, want 401", badResp.StatusCode)
	}
	if len(store.AuditLogs) == 0 || store.AuditLogs[len(store.AuditLogs)-1].GatewayUserID != "u1" || store.AuditLogs[len(store.AuditLogs)-1].SyntheticUserID != "gateway-user" {
		t.Fatalf("bad login audit was not associated with gateway user: %#v", store.AuditLogs)
	}

	loginResp := do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Pw":"alice-pass"}`))
	defer loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", loginResp.StatusCode)
	}
	var login map[string]any
	decodeJSON(t, loginResp.Body, &login)
	gatewayToken, _ := login["AccessToken"].(string)

	logoutReq := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Logout", nil)
	logoutReq.Header.Set("X-Emby-Token", gatewayToken)
	logoutResp := do(t, logoutReq)
	_ = logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d, want 200", logoutResp.StatusCode)
	}

	for _, event := range []string{"login_failure", "login_success", "logout_success"} {
		if !hasAuditEvent(store, event) {
			t.Fatalf("missing audit event %q in %#v", event, store.AuditLogs)
		}
	}
	auditJSON := mustJSON(t, store.AuditLogs)
	for _, secret := range []string{"alice-pass", "backend-token", gatewayToken} {
		if secret != "" && strings.Contains(auditJSON, secret) {
			t.Fatalf("audit log leaked secret %q: %s", secret, auditJSON)
		}
	}
}

func TestAuditLogsForAuthDependencyFailures(t *testing.T) {
	t.Run("mapping unavailable", func(t *testing.T) {
		store := testStore("http://127.0.0.1/emby")
		store.Mappings["u1"] = UserMapping{Enabled: false}
		gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
		defer gw.Close()

		resp := do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Pw":"alice-pass"}`))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("login status = %d, want 401", resp.StatusCode)
		}
		if !hasAuditEvent(store, "mapping_unavailable") {
			t.Fatalf("missing mapping_unavailable audit in %#v", store.AuditLogs)
		}
	})

	t.Run("backend auth failure", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "backend failed", http.StatusUnauthorized)
		}))
		defer backend.Close()
		store := testStore(backend.URL + "/emby")
		gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
		defer gw.Close()

		resp := do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Pw":"alice-pass"}`))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("login status = %d, want 502", resp.StatusCode)
		}
		if !hasAuditEvent(store, "backend_auth_failure") {
			t.Fatalf("missing backend_auth_failure audit in %#v", store.AuditLogs)
		}
	})

	t.Run("session save failure", func(t *testing.T) {
		backend := testAuthBackend(t)
		defer backend.Close()
		store := &failingSaveStore{MemoryStore: testStore(backend.URL + "/emby")}
		gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
		defer gw.Close()

		resp := do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Pw":"alice-pass"}`))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("login status = %d, want 500", resp.StatusCode)
		}
		if !hasAuditEvent(store.MemoryStore, "session_save_failure") {
			t.Fatalf("missing session_save_failure audit in %#v", store.AuditLogs)
		}
	})
}

func TestLoginFailureAuditAssociatesExistingUser(t *testing.T) {
	backend := testAuthBackend(t)
	defer backend.Close()
	store := testStore(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	missingResp := do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice"}`))
	_ = missingResp.Body.Close()
	if missingResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing password status = %d, want 400", missingResp.StatusCode)
	}
	last := store.AuditLogs[len(store.AuditLogs)-1]
	if last.Event != "login_failure" || last.GatewayUserID != "u1" || last.SyntheticUserID != "gateway-user" {
		t.Fatalf("missing credentials audit not associated with gateway user: %#v", last)
	}

	for i := 0; i < loginFailureLimit; i++ {
		resp := do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Pw":"bad"}`))
		_ = resp.Body.Close()
	}
	blockedResp := do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Pw":"alice-pass"}`))
	_ = blockedResp.Body.Close()
	if blockedResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("blocked login status = %d, want 401", blockedResp.StatusCode)
	}
	last = store.AuditLogs[len(store.AuditLogs)-1]
	if last.Event != "login_failure" || last.Message != "login blocked" || last.GatewayUserID != "u1" || last.SyntheticUserID != "gateway-user" {
		t.Fatalf("blocked audit not associated with gateway user: %#v", last)
	}
}

func TestPathPolicyDenyAndDefaultAllow(t *testing.T) {
	var openHits int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Items/Open":
			openHits++
			writeTestJSON(w, map[string]any{"ok": true})
		case "/emby/Items/Secret":
			t.Fatal("denied path reached backend")
		default:
			t.Fatalf("unexpected backend request %s", r.URL.Path)
		}
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	store.PathPolicies = []PathPolicy{{Method: http.MethodGet, Path: "/Items/Secret", Action: "deny", Enabled: true}}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	openResp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/Open?api_key=gateway-token", nil))
	_ = openResp.Body.Close()
	if openResp.StatusCode != http.StatusOK || openHits != 1 {
		t.Fatalf("default allow status = %d hits = %d, want 200/1", openResp.StatusCode, openHits)
	}

	deniedResp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/Secret?api_key=gateway-token", nil))
	_ = deniedResp.Body.Close()
	if deniedResp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied status = %d, want 403", deniedResp.StatusCode)
	}
	if !hasAuditEvent(store, "path_denied") {
		t.Fatalf("missing path_denied audit in %#v", store.AuditLogs)
	}

	store.PathPolicies = append(store.PathPolicies, PathPolicy{Method: http.MethodGet, Path: "/Users/Public", Action: "deny", Enabled: true})
	usersResp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/Public", nil))
	_ = usersResp.Body.Close()
	if usersResp.StatusCode != http.StatusForbidden {
		t.Fatalf("special handler denied status = %d, want 403", usersResp.StatusCode)
	}
}

func TestPlaybackEventsAndStateAreRecordedAndForwarded(t *testing.T) {
	const gatewayToken = "gateway-token"
	var forwarded []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		forwarded = append(forwarded, r.URL.Path+":"+string(body))
		if strings.Contains(string(body), "gateway-user") || !strings.Contains(string(body), "backend-user") {
			t.Fatalf("playback body was not mapped to backend user: %s", string(body))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken(gatewayToken)] = testSession(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	requests := []struct {
		path string
		body string
	}{
		{"/Sessions/Playing", `{"Item":{"Id":"item-1"},"UserId":"gateway-user","PositionTicks":100}`},
		{"/Sessions/Playing/Progress", `{"ItemId":"item-1","UserId":"gateway-user","PlaybackPositionTicks":250,"PlayedPercentage":50.5}`},
		{"/Sessions/Playing/Stopped", `{"ItemId":"item-1","UserId":"gateway-user","PositionTicks":500,"Played":true,"PlayedPercentage":95}`},
	}
	for _, req := range requests {
		httpReq := mustRequest(t, http.MethodPost, gw.URL+"/emby"+req.path+"?api_key="+gatewayToken, strings.NewReader(req.body))
		httpReq.Header.Set("Content-Type", "application/json")
		resp := do(t, httpReq)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s status = %d, want 204", req.path, resp.StatusCode)
		}
	}
	if len(forwarded) != 3 || len(store.PlaybackEvents) != 3 {
		t.Fatalf("forwarded=%d events=%d, want 3/3", len(forwarded), len(store.PlaybackEvents))
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "item-1")
	if err != nil {
		t.Fatalf("find playback state: %v", err)
	}
	if !state.Played || state.PlayCount != 1 || state.PlaybackPositionTicks != 500 || state.PlayedPercentage == nil || *state.PlayedPercentage != 95 || state.LastPlayedDate == nil {
		t.Fatalf("unexpected playback state: %#v", state)
	}
	if _, err := store.FindSessionByTokenHash(context.Background(), HashToken(gatewayToken)); err != nil {
		t.Fatalf("session missing after playback: %v", err)
	}
	playbackJSON := mustJSON(t, store.PlaybackEvents)
	if strings.Contains(playbackJSON, gatewayToken) || strings.Contains(playbackJSON, "backend-token") || strings.Contains(playbackJSON, "alice-pass") {
		t.Fatalf("playback event leaked secret: %s", playbackJSON)
	}
}

func TestUserDataVirtualizationIsGatewayUserScoped(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"Item": map[string]any{
				"Id":       "item-1",
				"UserData": map[string]any{"Played": true, "PlaybackPositionTicks": float64(9999), "PlayedPercentage": float64(99), "PlayCount": float64(9)},
			},
			"Items": []any{
				map[string]any{"Id": "item-1", "UserData": map[string]any{"Played": true, "PlaybackPositionTicks": float64(9999), "PlayedPercentage": float64(99), "PlayCount": float64(9)}},
			},
		})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("token-u1")] = testSession(backend.URL + "/emby")
	u2Session := *testSession(backend.URL + "/emby")
	u2Session.GatewayTokenHash = HashToken("token-u2")
	u2Session.GatewayUserID = "u2"
	u2Session.GatewayUsername = "bob"
	u2Session.SyntheticUserID = "gateway-user-2"
	store.Sessions[u2Session.GatewayTokenHash] = &u2Session
	u3Session := *testSession(backend.URL + "/emby")
	u3Session.GatewayTokenHash = HashToken("token-u3")
	u3Session.GatewayUserID = "u3"
	u3Session.GatewayUsername = "charlie"
	u3Session.SyntheticUserID = "gateway-user-3"
	store.Sessions[u3Session.GatewayTokenHash] = &u3Session
	pct1 := 42.5
	pct2 := 88.25
	lastPlayed := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", ItemID: "item-1", PlaybackPositionTicks: 4200, PlayedPercentage: &pct1, Played: false, PlayCount: 1})
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u2", ItemID: "item-1", PlaybackPositionTicks: 8800, PlayedPercentage: &pct2, Played: true, LastPlayedDate: &lastPlayed, PlayCount: 3})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	u1 := fetchUserData(t, gw.URL+"/emby/Items/item-1?api_key=token-u1")
	u2 := fetchUserData(t, gw.URL+"/emby/Items/item-1?api_key=token-u2")
	u3 := fetchUserData(t, gw.URL+"/emby/Items/item-1?api_key=token-u3")
	if u1["Played"] != false || int(u1["PlaybackPositionTicks"].(float64)) != 4200 || u1["PlayedPercentage"].(float64) != pct1 || int(u1["PlayCount"].(float64)) != 1 {
		t.Fatalf("unexpected u1 user data: %#v", u1)
	}
	if u2["Played"] != true || int(u2["PlaybackPositionTicks"].(float64)) != 8800 || u2["PlayedPercentage"].(float64) != pct2 || int(u2["PlayCount"].(float64)) != 3 || u2["LastPlayedDate"] == "" {
		t.Fatalf("unexpected u2 user data: %#v", u2)
	}
	if u3["Played"] != false || int(u3["PlaybackPositionTicks"].(float64)) != 0 || u3["PlayedPercentage"] != nil || int(u3["PlayCount"].(float64)) != 0 || u3["LastPlayedDate"] != nil {
		t.Fatalf("missing state should not leak backend user data: %#v", u3)
	}
}

func TestResumeUsesGatewayStateAndResolvesExistingItems(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Users/backend-user/Items" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		if r.URL.Query().Get("Ids") != "item-u1,missing-item" {
			t.Fatalf("backend Ids = %q, want user scoped resume ids", r.URL.Query().Get("Ids"))
		}
		writeTestJSON(w, map[string]any{"Items": []any{
			map[string]any{"Id": "item-u1", "Name": "Episode 1", "Type": "Episode", "UserData": map[string]any{"PlaybackPositionTicks": float64(999)}},
		}})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("token-u1")] = testSession(backend.URL + "/emby")
	u2 := *testSession(backend.URL + "/emby")
	u2.GatewayTokenHash = HashToken("token-u2")
	u2.GatewayUserID = "u2"
	u2.SyntheticUserID = "gateway-user-2"
	store.Sessions[u2.GatewayTokenHash] = &u2
	pct := 12.5
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "item-u1", PlaybackPositionTicks: 1200, PlayedPercentage: &pct, UpdatedAt: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)})
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "missing-item", PlaybackPositionTicks: 800, UpdatedAt: time.Date(2026, 7, 8, 11, 0, 0, 0, time.UTC)})
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u2", SyntheticUserID: "gateway-user-2", ItemID: "item-u2", PlaybackPositionTicks: 9999})

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items/Resume?api_key=token-u1", nil))
	defer resp.Body.Close()
	var body map[string]any
	decodeJSON(t, resp.Body, &body)
	items := body["Items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["Id"] != "item-u1" {
		t.Fatalf("resume items = %#v, want only item-u1", items)
	}
	userData := items[0].(map[string]any)["UserData"].(map[string]any)
	if int(userData["PlaybackPositionTicks"].(float64)) != 1200 || userData["PlayedPercentage"].(float64) != pct {
		t.Fatalf("resume UserData not overlaid: %#v", userData)
	}
	missing, err := store.FindPlaybackState(context.Background(), "u1", "missing-item")
	if err != nil || missing.OrphanedAt == nil {
		t.Fatalf("missing item was not marked orphaned: %#v err=%v", missing, err)
	}
}

func TestPersonalStateWritesAreTerminatedAtGateway(t *testing.T) {
	var backendRequests int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendRequests++
		t.Fatalf("personal state write should not reach backend: %s %s", r.Method, r.URL.String())
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	requests := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/Users/gateway-user/PlayedItems/item-1", ""},
		{http.MethodPost, "/Users/gateway-user/FavoriteItems/item-1", ""},
		{http.MethodPost, "/Users/gateway-user/Items/item-1/Rating?Likes=true", ""},
		{http.MethodPost, "/Users/gateway-user/Items/item-1/UserData", `{"PlaybackPositionTicks":321,"PlayedPercentage":33.3}`},
	}
	for _, tc := range requests {
		req := mustRequest(t, tc.method, gw.URL+"/emby"+tc.path+"&api_key=gateway-token", strings.NewReader(tc.body))
		if !strings.Contains(tc.path, "?") {
			req = mustRequest(t, tc.method, gw.URL+"/emby"+tc.path+"?api_key=gateway-token", strings.NewReader(tc.body))
		}
		req.Header.Set("Content-Type", "application/json")
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s status = %d", tc.method, tc.path, resp.StatusCode)
		}
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "item-1")
	if err != nil {
		t.Fatalf("find item state: %v", err)
	}
	if !state.Played || !state.IsFavorite || state.Likes == nil || !*state.Likes || state.PlaybackPositionTicks != 321 {
		t.Fatalf("personal state not persisted: %#v", state)
	}
	if backendRequests != 0 {
		t.Fatalf("backendRequests = %d, want 0", backendRequests)
	}
}

func TestPersonalFiltersTranslateToBackendIDSets(t *testing.T) {
	var sawIDs string
	var sawFilters string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Users/backend-user/Items" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		sawIDs = r.URL.Query().Get("Ids")
		sawFilters = r.URL.Query().Get("Filters")
		writeTestJSON(w, map[string]any{"Items": []any{map[string]any{"Id": sawIDs, "UserData": map[string]any{}}}, "TotalRecordCount": 1})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "fav-1", IsFavorite: true})
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "plain-1"})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items?api_key=gateway-token&Filters=IsFavorite", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filtered items status = %d", resp.StatusCode)
	}
	if sawIDs != "fav-1" || sawFilters != "" {
		t.Fatalf("backend query Ids=%q Filters=%q, want fav-1/no personal filter", sawIDs, sawFilters)
	}
}

func TestNextUpUsesGatewaySeriesState(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Shows/show-1/Episodes" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		writeTestJSON(w, map[string]any{"Items": []any{
			map[string]any{"Id": "ep-1", "Name": "Episode 1", "Type": "Episode", "SeriesId": "show-1", "ParentIndexNumber": 1, "IndexNumber": 1, "UserData": map[string]any{}},
			map[string]any{"Id": "ep-2", "Name": "Episode 2", "Type": "Episode", "SeriesId": "show-1", "ParentIndexNumber": 1, "IndexNumber": 2, "UserData": map[string]any{}},
		}})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	lastPlayed := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "ep-1", SeriesID: "show-1", ParentIndexNumber: 1, IndexNumber: 1, Played: true, LastPlayedDate: &lastPlayed})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Shows/NextUp?api_key=gateway-token", nil))
	defer resp.Body.Close()
	var body map[string]any
	decodeJSON(t, resp.Body, &body)
	items := body["Items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["Id"] != "ep-2" {
		t.Fatalf("next up items = %#v, want ep-2", items)
	}
}

func TestDisplayPreferencesAndSessionsAreUserScoped(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Sessions" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		writeTestJSON(w, []any{
			map[string]any{"DeviceId": "device-1", "NowPlayingItem": map[string]any{"Id": "visible"}},
			map[string]any{"DeviceId": "device-2", "NowPlayingItem": map[string]any{"Id": "hidden"}},
		})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	session := testSession(backend.URL + "/emby")
	session.DeviceID = "device-1"
	store.Sessions[HashToken("gateway-token")] = session
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	prefReq := mustRequest(t, http.MethodPost, gw.URL+"/emby/DisplayPreferences/home?api_key=gateway-token&Client=web", strings.NewReader(`{"SortBy":"DateCreated"}`))
	prefReq.Header.Set("Content-Type", "application/json")
	prefResp := do(t, prefReq)
	_ = prefResp.Body.Close()
	if prefResp.StatusCode != http.StatusOK {
		t.Fatalf("save preferences status = %d", prefResp.StatusCode)
	}
	getPref := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/DisplayPreferences/home?api_key=gateway-token&Client=web", nil))
	defer getPref.Body.Close()
	prefBody, _ := io.ReadAll(getPref.Body)
	if !strings.Contains(string(prefBody), "DateCreated") {
		t.Fatalf("preference body = %s", string(prefBody))
	}

	sessionsResp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions?api_key=gateway-token", nil))
	defer sessionsResp.Body.Close()
	var sessions []any
	decodeJSON(t, sessionsResp.Body, &sessions)
	if len(sessions) != 1 || sessions[0].(map[string]any)["DeviceId"] != "device-1" {
		t.Fatalf("sessions = %#v, want only device-1", sessions)
	}
}

func TestLoginFailureRateLimitIsRemoteIPScoped(t *testing.T) {
	backend := testAuthBackend(t)
	defer backend.Close()

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, testStore(backend.URL+"/emby")))
	defer gw.Close()

	loginURL := gw.URL + "/emby/Users/AuthenticateByName"
	for i := 0; i < loginFailureLimit; i++ {
		req := mustJSONLoginRequest(t, loginURL, `{"Username":"alice","Pw":"bad"}`)
		req.Header.Set("X-Forwarded-For", "203.0.113.10")
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("bad login %d status = %d, want 401", i, resp.StatusCode)
		}
	}

	blockedReq := mustJSONLoginRequest(t, loginURL, `{"Username":"alice","Pw":"alice-pass"}`)
	blockedReq.Header.Set("X-Forwarded-For", "203.0.113.10")
	blockedResp := do(t, blockedReq)
	_ = blockedResp.Body.Close()
	if blockedResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("blocked IP login status = %d, want 401", blockedResp.StatusCode)
	}

	isolatedReq := mustJSONLoginRequest(t, loginURL, `{"Username":"alice","Pw":"alice-pass"}`)
	isolatedReq.Header.Set("X-Forwarded-For", "203.0.113.11")
	isolatedResp := do(t, isolatedReq)
	_ = isolatedResp.Body.Close()
	if isolatedResp.StatusCode != http.StatusOK {
		t.Fatalf("different IP login status = %d, want 200", isolatedResp.StatusCode)
	}
}

func TestConcurrentFailedLoginsDoNotCrash(t *testing.T) {
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, testStore("http://127.0.0.1/emby")))
	defer gw.Close()

	const attempts = 64
	var wg sync.WaitGroup
	errs := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, gw.URL+"/emby/Users/AuthenticateByName", strings.NewReader(`{"Username":"alice","Pw":"bad"}`))
			if err != nil {
				errs <- err
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := testHTTPClient.Do(req)
			if err != nil {
				errs <- err
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				errs <- errors.New("concurrent bad login returned non-401 status")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestLogoutReturnsErrorWhenRevokeFails(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	store := &failingRevokeStore{MemoryStore: NewMemoryStore()}
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Logout", nil)
	req.Header.Set("X-Emby-Token", "gateway-token")
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("logout status = %d, want 500", resp.StatusCode)
	}
}

func TestOversizedJSONDoesNotPassTruncatedBody(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"leak":"backend-token","padding":"` + strings.Repeat("x", proxyJSONLimit) + `"}`))
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	req := mustRequest(t, http.MethodGet, gw.URL+"/emby/large?api_key=gateway-token", nil)
	resp := do(t, req)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("oversized json status = %d, want 502", resp.StatusCode)
	}
	if strings.Contains(string(body), "backend-token") || strings.Contains(string(body), strings.Repeat("x", 128)) {
		t.Fatalf("oversized json body included backend/truncated content")
	}
	if !hasAuditEvent(store, "proxy_read_failed") {
		t.Fatalf("missing proxy_read_failed audit in %#v", store.AuditLogs)
	}
}

func TestProxyBackendUnavailableIsAudited(t *testing.T) {
	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession("http://127.0.0.1:1/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/System/Info?api_key=gateway-token", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("proxy status = %d, want 502", resp.StatusCode)
	}
	if !hasAuditEvent(store, "proxy_backend_unavailable") {
		t.Fatalf("missing proxy_backend_unavailable audit in %#v", store.AuditLogs)
	}
}

func TestOctetStreamM3U8IsRewritten(t *testing.T) {
	var backendURL string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("#EXTM3U\n" + backendURL + "/emby/seg.ts?api_key=backend-token\n"))
	}))
	defer backend.Close()
	backendURL = backend.URL

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{PublicBaseURL: "https://media.example.com", GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/master.m3u8?api_key=gateway-token", nil))
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("m3u8 status = %d", resp.StatusCode)
	}
	if strings.Contains(text, backend.URL) || strings.Contains(text, "backend-token") {
		t.Fatalf("m3u8 leaked backend details: %s", text)
	}
	if !strings.Contains(text, "https://media.example.com/emby/seg.ts?api_key=gateway-token") {
		t.Fatalf("m3u8 was not rewritten: %s", text)
	}
}

func TestOversizedM3U8DoesNotPassTruncatedBody(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte("#EXTM3U\nbackend-token\n" + strings.Repeat("x", proxyM3U8Limit)))
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/large.m3u8?api_key=gateway-token", nil))
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("oversized m3u8 status = %d, want 502", resp.StatusCode)
	}
	if strings.Contains(string(body), "backend-token") || strings.Contains(string(body), strings.Repeat("x", 128)) {
		t.Fatalf("oversized m3u8 body included backend/truncated content")
	}
}

func TestGatewayBasePathCanBeChanged(t *testing.T) {
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/media"}, NewMemoryStore()))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/media/System/Ping", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("custom base path ping status = %d", resp.StatusCode)
	}
	resp = do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/System/Ping", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("old base path status = %d, want 404", resp.StatusCode)
	}
}

type failingRevokeStore struct {
	*MemoryStore
}

func (f *failingRevokeStore) RevokeSession(ctx context.Context, tokenHash string) error {
	return errors.New("revoke failed")
}

type failingSaveStore struct {
	*MemoryStore
}

func (f *failingSaveStore) SaveSession(ctx context.Context, session *Session) error {
	return errors.New("save failed")
}

func hasAuditEvent(store *MemoryStore, event string) bool {
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, entry := range store.AuditLogs {
		if entry.Event == event {
			return true
		}
	}
	return false
}

func fetchUserData(t *testing.T, url string) map[string]any {
	t.Helper()
	resp := do(t, mustRequest(t, http.MethodGet, url, nil))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("user data response status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	decodeJSON(t, resp.Body, &body)
	item, ok := body["Item"].(map[string]any)
	if !ok {
		t.Fatalf("missing Item in %#v", body)
	}
	userData, ok := item["UserData"].(map[string]any)
	if !ok {
		t.Fatalf("missing UserData in %#v", item)
	}
	items, ok := body["Items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("missing Items in %#v", body)
	}
	listItem, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected Items[0] in %#v", items[0])
	}
	listUserData, ok := listItem["UserData"].(map[string]any)
	if !ok || listUserData["PlaybackPositionTicks"] != userData["PlaybackPositionTicks"] {
		t.Fatalf("nested list UserData was not virtualized: item=%#v list=%#v", userData, listUserData)
	}
	return userData
}

func testStore(backendBaseURL string) *MemoryStore {
	store := NewMemoryStore()
	store.Users["u1"] = MemoryUser{
		GatewayUser: GatewayUser{ID: "u1", Username: "alice", SyntheticUserID: "gateway-user", Enabled: true},
		Password:    "alice-pass",
	}
	store.Mappings["u1"] = UserMapping{
		ID:               "m1",
		GatewayUserID:    "u1",
		BackendAccountID: "b1",
		Enabled:          true,
		BackendAccount: BackendAccount{
			ID:       "b1",
			ServerID: "s1",
			BaseURL:  backendBaseURL,
			Username: "shared",
			Password: "backend-pass",
			Enabled:  true,
		},
	}
	return store
}

func testSession(backendBaseURL string) *Session {
	return &Session{
		GatewayTokenHash: HashToken("gateway-token"),
		GatewayUserID:    "u1",
		GatewayUsername:  "alice",
		SyntheticUserID:  "gateway-user",
		BackendAccountID: "b1",
		BackendServerID:  "backend-server",
		BackendBaseURL:   backendBaseURL,
		BackendUserID:    "backend-user",
		BackendUsername:  "shared",
		BackendToken:     "backend-token",
		CreatedAt:        time.Now().UTC(),
		ExpiresAt:        time.Now().UTC().Add(time.Hour),
	}
}

func testAuthBackend(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/emby/Users/AuthenticateByName" {
			t.Fatalf("unexpected backend request %s %s", r.Method, r.URL.String())
		}
		writeTestJSON(w, map[string]any{
			"AccessToken": "backend-token",
			"ServerId":    "backend-server",
			"User": map[string]any{
				"Id":   "backend-user",
				"Name": "shared",
			},
		})
	}))
}

func mustJSONLoginRequest(t *testing.T, url, body string) *http.Request {
	t.Helper()
	req := mustRequest(t, http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := testHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func mustRequest(t *testing.T, method, url string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}

func decodeJSON(t *testing.T, r io.Reader, value any) {
	t.Helper()
	if err := json.NewDecoder(r).Decode(value); err != nil {
		t.Fatalf("decode json: %v", err)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return string(b)
}
