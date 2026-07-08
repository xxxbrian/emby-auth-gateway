package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayMVPTokenMappingAndRewriting(t *testing.T) {
	const (
		backendToken    = "backend-token-secret"
		backendUserID   = "backend-user-id"
		backendServerID = "backend-server-id"
		syntheticUserID = "gateway-user-id"
	)

	var backendURL string
	var sawControlledBackendLogin bool
	var sawBackendTokenInRequest bool
	var sawBackendUserInPath bool
	var sawBackendTokenFromAPIKey bool

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/emby/Users/AuthenticateByName":
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
				"LocalAddress":    backendURL + "/emby",
				"WanAddress":      backendURL + "/emby",
				"RemoteAddresses": []string{backendURL + "/emby"},
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
	loginReq.Header.Set("X-Emby-Authorization", `Emby Client="Test", Device="curl", DeviceId="dev-1", Version="1"`)
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

func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
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
