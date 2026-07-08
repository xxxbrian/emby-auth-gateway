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
