package gateway

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var testHTTPClient = &http.Client{Timeout: 5 * time.Second}

func TestWriteProxyResponseRejectsEmptyImage(t *testing.T) {
	store := NewMemoryStore()
	server := NewServer(Config{GatewayBasePath: "/emby"}, store)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item-1/Images/Primary", nil)
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"image/jpeg"}, "Cache-Control": []string{"public, max-age=604800"}},
		Body:          io.NopCloser(strings.NewReader("")),
		ContentLength: 0,
		Request:       req,
	}
	recorder := httptest.NewRecorder()

	server.writeProxyResponse(recorder, req, "/Items/item-1/Images/Primary", resp, &Session{}, "", "")

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := recorder.Header().Get("ETag"); got != "" {
		t.Fatalf("ETag = %q, want empty", got)
	}
}

func TestWriteProxyResponseAbortsTruncatedImage(t *testing.T) {
	store := NewMemoryStore()
	server := NewServer(Config{GatewayBasePath: "/emby"}, store)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item-1/Images/Primary", nil)
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"image/jpeg"}},
		Body:          &truncatedReadCloser{data: []byte("ab")},
		ContentLength: 4,
		Request:       req,
	}
	recorder := httptest.NewRecorder()

	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("panic = %#v, want http.ErrAbortHandler", got)
		}
		if got := recorder.Code; got != http.StatusOK {
			t.Fatalf("status = %d, want %d", got, http.StatusOK)
		}
		if got := recorder.Header().Get("Content-Length"); got != "4" {
			t.Fatalf("Content-Length = %q, want 4", got)
		}
	}()
	server.writeProxyResponse(recorder, req, "/Items/item-1/Images/Primary", resp, &Session{}, "", "")
}

func TestWriteProxyResponseAllowsBodylessImageResponses(t *testing.T) {
	tests := []struct {
		name   string
		method string
		status int
		length int64
	}{
		{name: "head", method: http.MethodHead, status: http.StatusOK, length: 1234},
		{name: "not modified", method: http.MethodGet, status: http.StatusNotModified, length: 0},
		{name: "no content", method: http.MethodGet, status: http.StatusNoContent, length: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewMemoryStore()
			server := NewServer(Config{GatewayBasePath: "/emby"}, store)
			req := httptest.NewRequest(tt.method, "http://gateway.test/emby/Items/item-1/Images/Primary", nil)
			resp := &http.Response{
				StatusCode:    tt.status,
				Header:        http.Header{"Content-Type": []string{"image/jpeg"}, "ETag": []string{`"tag"`}},
				Body:          panicReadCloser{},
				ContentLength: tt.length,
				Request:       req,
			}
			recorder := httptest.NewRecorder()

			server.writeProxyResponse(recorder, req, "/Items/item-1/Images/Primary", resp, &Session{}, "", "")

			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.status)
			}
			if recorder.Body.Len() != 0 {
				t.Fatalf("body length = %d, want 0", recorder.Body.Len())
			}
			if got := recorder.Header().Get("ETag"); got != `"tag"` {
				t.Fatalf("ETag = %q, want tag", got)
			}
		})
	}
}

func TestWriteProxyResponseValidatesCompleteImage(t *testing.T) {
	validJPEG := []byte{0xff, 0xd8, 1, 2, 3, 0xff, 0xd9}
	tests := []struct {
		name         string
		status       int
		requestHead  http.Header
		responseHead http.Header
		body         []byte
		wantAbort    bool
	}{
		{name: "valid jpeg", status: http.StatusOK, body: validJPEG},
		{name: "clean eof truncated jpeg", status: http.StatusOK, body: []byte{0xff, 0xd8, 1, 2, 3}, wantAbort: true},
		{name: "partial content", status: http.StatusPartialContent, requestHead: http.Header{"Range": []string{"bytes=0-3"}}, responseHead: http.Header{"Content-Range": []string{"bytes 0-3/100"}}, body: []byte{0xff, 0xd8, 1, 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewMemoryStore()
			server := NewServer(Config{GatewayBasePath: "/emby"}, store)
			req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item-1/Images/Primary", nil)
			req.Header = tt.requestHead
			header := tt.responseHead.Clone()
			if header == nil {
				header = http.Header{}
			}
			header.Set("Content-Type", "image/jpeg")
			resp := &http.Response{StatusCode: tt.status, Header: header, Body: io.NopCloser(strings.NewReader(string(tt.body))), ContentLength: int64(len(tt.body)), Request: req}
			recorder := httptest.NewRecorder()

			aborted := false
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						if recovered != http.ErrAbortHandler {
							t.Fatalf("panic = %#v, want http.ErrAbortHandler", recovered)
						}
						aborted = true
					}
				}()
				server.writeProxyResponse(recorder, req, "/Items/item-1/Images/Primary", resp, &Session{}, "", "")
			}()

			if aborted != tt.wantAbort {
				t.Fatalf("aborted = %v, want %v", aborted, tt.wantAbort)
			}
			if !tt.wantAbort && !bytes.Equal(recorder.Body.Bytes(), tt.body) {
				t.Fatalf("body = %x, want %x", recorder.Body.Bytes(), tt.body)
			}
		})
	}
}

type panicReadCloser struct{}

func (panicReadCloser) Read([]byte) (int, error) { panic("body must not be read") }
func (panicReadCloser) Close() error             { return nil }

type truncatedReadCloser struct {
	data []byte
	off  int
}

func (r *truncatedReadCloser) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

func (r *truncatedReadCloser) Close() error { return nil }

func TestGatewayMVPTokenMappingAndRewriting(t *testing.T) {
	const (
		backendToken    = "backend-token-secret"
		backendUserID   = "backend-user-id"
		backendServerID = "backend-server-id"
		syntheticUserID = "gateway-user-id"
	)

	var backendURL string
	const backendDeviceID = "11111111-2222-4333-8444-555555555555"
	var sawControlledBackendLogin bool
	var sawBackendAuthUserAgent bool
	var sawBackendAuthIdentity bool
	var sawProxyUserAgent bool
	var sawProxyIdentity bool
	var sawBackendTokenInRequest bool
	var sawBackendUserInPath bool
	var sawBackendTokenFromAPIKey bool

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/emby/Users/AuthenticateByName":
			if r.UserAgent() == "SenPlayer/6.1.3" {
				sawBackendAuthUserAgent = true
			}
			auth := ParseEmbyAuthHeader(r.Header.Get("X-Emby-Authorization"))
			if auth.Client == "SenPlayer" && auth.Device == "Mac" && auth.DeviceID == backendDeviceID && auth.Version == "6.1.3" {
				sawBackendAuthIdentity = true
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
			if r.UserAgent() == "SenPlayer/6.1.3" {
				sawProxyUserAgent = true
			}
			auth := ParseEmbyAuthHeader(r.Header.Get("X-Emby-Authorization"))
			if auth.Client == "SenPlayer" && auth.Device == "Mac" && auth.DeviceID == backendDeviceID && auth.Version == "6.1.3" && auth.UserID == backendUserID && auth.Token == backendToken {
				sawProxyIdentity = true
			}
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
			ID:             "b1",
			ServerID:       "s1",
			BaseURL:        backend.URL + "/emby",
			Username:       "shared",
			Password:       "backend-pass",
			Enabled:        true,
			ClientIdentity: backendIdentityForTest(backendDeviceID),
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
	loginReq.Header.Set("User-Agent", "DifferentClient/1.0")
	loginReq.Header.Set("X-Emby-Authorization", `Emby Client="Different", Device="Phone", DeviceId="client-device", Version="1.0"`)
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
	if sawControlledBackendLogin || sawBackendAuthUserAgent || sawBackendAuthIdentity {
		t.Fatal("gateway login unexpectedly authenticated to backend")
	}
	if strings.Contains(mustJSON(t, login), backendToken) || strings.Contains(mustJSON(t, login), backendUserID) {
		t.Fatalf("login leaked backend token or user id: %s", mustJSON(t, login))
	}
	loginUser, ok := login["User"].(map[string]any)
	if !ok {
		t.Fatalf("login User missing: %#v", login)
	}
	if _, ok := loginUser["Policy"].(map[string]any); !ok {
		t.Fatalf("login User.Policy missing: %#v", loginUser)
	}
	if _, ok := loginUser["Configuration"].(map[string]any); !ok {
		t.Fatalf("login User.Configuration missing: %#v", loginUser)
	}
	if _, ok := login["SessionInfo"].(map[string]any); !ok {
		t.Fatalf("login SessionInfo missing: %#v", login)
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
	if !sawControlledBackendLogin {
		t.Fatal("lazy backend authentication did not use controlled backend account credentials")
	}
	if !sawBackendAuthUserAgent {
		t.Fatal("backend authentication did not receive configured user agent")
	}
	if !sawBackendAuthIdentity {
		t.Fatal("backend authentication did not receive configured authorization identity")
	}
	if !sawProxyUserAgent || !sawProxyIdentity {
		t.Fatal("backend proxy request did not receive configured identity")
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

func TestProxyRefreshesBackendTokenOnUnauthorized(t *testing.T) {
	const syntheticUserID = "gateway-user"
	var refreshCount int
	var logoutCount int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/emby/Users/AuthenticateByName":
			refreshCount++
			writeTestJSON(w, map[string]any{
				"AccessToken": "backend-token-2",
				"ServerId":    "backend-server",
				"User": map[string]any{
					"Id":   "backend-user",
					"Name": "shared",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/emby/System/Info":
			if r.Header.Get("X-Emby-Token") == "backend-token-1" {
				http.Error(w, "stale", http.StatusUnauthorized)
				return
			}
			if r.Header.Get("X-Emby-Token") != "backend-token-2" {
				t.Fatalf("unexpected backend token %q", r.Header.Get("X-Emby-Token"))
			}
			writeTestJSON(w, map[string]any{"Id": "backend-server", "UserId": "backend-user"})
		case r.Method == http.MethodPost && r.URL.Path == "/emby/Sessions/Logout":
			logoutCount++
			if r.Header.Get("X-Emby-Token") != "backend-token-1" {
				t.Fatalf("logout token = %q, want old backend token", r.Header.Get("X-Emby-Token"))
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected backend request %s %s", r.Method, r.URL.String())
		}
	}))
	defer backend.Close()

	store := testStore(backend.URL + "/emby")
	store.Users["u1"] = MemoryUser{GatewayUser: GatewayUser{ID: "u1", Username: "alice", SyntheticUserID: syntheticUserID, Enabled: true}, Password: "alice-pass"}
	mapping := store.Mappings["u1"]
	mapping.BackendAccount.Server.ServerVersion = "4.9.5.0"
	mapping.BackendAccount.Server.ServerName = "Probed Emby"
	mapping.BackendAccount.BackendToken = "backend-token-1"
	mapping.BackendAccount.BackendUserID = "backend-user"
	oldTokenTime := time.Now().UTC().Add(-backendTokenRefreshMinInterval - time.Second)
	mapping.BackendAccount.TokenUpdatedAt = &oldTokenTime
	store.Mappings["u1"] = mapping
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", GatewayServerID: "gateway-server"}, store))
	defer gw.Close()

	loginResp := do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Pw":"alice-pass"}`))
	var login map[string]any
	decodeJSON(t, loginResp.Body, &login)
	_ = loginResp.Body.Close()
	token, _ := login["AccessToken"].(string)
	req := mustRequest(t, http.MethodGet, gw.URL+"/emby/System/Info", nil)
	req.Header.Set("X-Emby-Token", token)
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("proxy status = %d: %s", resp.StatusCode, string(body))
	}
	if refreshCount != 1 {
		t.Fatalf("backend refresh count = %d, want 1", refreshCount)
	}
	if logoutCount != 1 {
		t.Fatalf("backend logout count = %d, want 1", logoutCount)
	}
	if !hasAuditEvent(store, "backend_token_refresh") {
		t.Fatalf("missing backend token refresh audit log: %#v", store.AuditLogs)
	}
	if store.Mappings["u1"].BackendAccount.BackendToken != "backend-token-2" {
		t.Fatalf("backend token was not refreshed in store: %#v", store.Mappings["u1"].BackendAccount)
	}
	if store.Mappings["u1"].BackendAccount.Server.ServerVersion != "4.9.5.0" || store.Mappings["u1"].BackendAccount.Server.ServerName != "Probed Emby" {
		t.Fatalf("lazy login cleared probed server info: %#v", store.Mappings["u1"].BackendAccount.Server)
	}
}

func TestProxyDoesNotRefreshWhenUnauthorizedTokenStillWorks(t *testing.T) {
	const syntheticUserID = "gateway-user"
	var authCount int
	var playbackCount int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/emby/Users/AuthenticateByName":
			authCount++
			writeTestJSON(w, map[string]any{
				"AccessToken": "backend-token",
				"ServerId":    "backend-server",
				"User": map[string]any{
					"Id":   "backend-user",
					"Name": "shared",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/emby/Items/item-1/PlaybackInfo":
			playbackCount++
			http.Error(w, "playback access denied", http.StatusUnauthorized)
		case r.Method == http.MethodGet && r.URL.Path == "/emby/System/Info":
			writeTestJSON(w, map[string]any{"Id": "backend-server"})
		default:
			t.Fatalf("unexpected backend request %s %s", r.Method, r.URL.String())
		}
	}))
	defer backend.Close()

	store := testStore(backend.URL + "/emby")
	store.Users["u1"] = MemoryUser{GatewayUser: GatewayUser{ID: "u1", Username: "alice", SyntheticUserID: syntheticUserID, Enabled: true}, Password: "alice-pass"}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", GatewayServerID: "gateway-server"}, store))
	defer gw.Close()

	loginResp := do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Pw":"alice-pass"}`))
	var login map[string]any
	decodeJSON(t, loginResp.Body, &login)
	_ = loginResp.Body.Close()
	gatewayToken, _ := login["AccessToken"].(string)

	req := mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/item-1/PlaybackInfo", nil)
	req.Header.Set("X-Emby-Token", gatewayToken)
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("proxy status = %d, want 401: %s", resp.StatusCode, string(body))
	}
	if authCount != 1 || playbackCount != 1 {
		t.Fatalf("auth/playback counts = %d/%d, want 1/1", authCount, playbackCount)
	}
}

func TestProxyDoesNotRefreshRecentUnauthorizedToken(t *testing.T) {
	const syntheticUserID = "gateway-user"
	var authCount int
	var playbackCount int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/emby/Users/AuthenticateByName":
			authCount++
			writeTestJSON(w, map[string]any{
				"AccessToken": "backend-token",
				"ServerId":    "backend-server",
				"User": map[string]any{
					"Id":   "backend-user",
					"Name": "shared",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/emby/Items/item-1/PlaybackInfo":
			playbackCount++
			http.Error(w, "stale", http.StatusUnauthorized)
		case r.Method == http.MethodGet && r.URL.Path == "/emby/System/Info":
			http.Error(w, "stale", http.StatusUnauthorized)
		default:
			t.Fatalf("unexpected backend request %s %s", r.Method, r.URL.String())
		}
	}))
	defer backend.Close()

	store := testStore(backend.URL + "/emby")
	store.Users["u1"] = MemoryUser{GatewayUser: GatewayUser{ID: "u1", Username: "alice", SyntheticUserID: syntheticUserID, Enabled: true}, Password: "alice-pass"}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", GatewayServerID: "gateway-server"}, store))
	defer gw.Close()

	loginResp := do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Pw":"alice-pass"}`))
	var login map[string]any
	decodeJSON(t, loginResp.Body, &login)
	_ = loginResp.Body.Close()
	gatewayToken, _ := login["AccessToken"].(string)

	req := mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/item-1/PlaybackInfo", nil)
	req.Header.Set("X-Emby-Token", gatewayToken)
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("proxy status = %d, want 401: %s", resp.StatusCode, string(body))
	}
	if authCount != 1 || playbackCount != 1 {
		t.Fatalf("auth/playback counts = %d/%d, want 1/1", authCount, playbackCount)
	}
}

func TestProxyDoesNotRetryWhenRefreshReturnsSameToken(t *testing.T) {
	const syntheticUserID = "gateway-user"
	var refreshCount int
	var playbackCount int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/emby/Users/AuthenticateByName":
			refreshCount++
			writeTestJSON(w, map[string]any{
				"AccessToken": "backend-token",
				"ServerId":    "backend-server",
				"User": map[string]any{
					"Id":   "backend-user",
					"Name": "shared",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/emby/Items/item-1/PlaybackInfo":
			playbackCount++
			http.Error(w, "stale", http.StatusUnauthorized)
		case r.Method == http.MethodGet && r.URL.Path == "/emby/System/Info":
			http.Error(w, "stale", http.StatusUnauthorized)
		default:
			t.Fatalf("unexpected backend request %s %s", r.Method, r.URL.String())
		}
	}))
	defer backend.Close()

	store := testStore(backend.URL + "/emby")
	store.Users["u1"] = MemoryUser{GatewayUser: GatewayUser{ID: "u1", Username: "alice", SyntheticUserID: syntheticUserID, Enabled: true}, Password: "alice-pass"}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", GatewayServerID: "gateway-server"}, store))
	defer gw.Close()

	loginResp := do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Pw":"alice-pass"}`))
	var login map[string]any
	decodeJSON(t, loginResp.Body, &login)
	_ = loginResp.Body.Close()
	gatewayToken, _ := login["AccessToken"].(string)
	oldTokenTime := time.Now().UTC().Add(-backendTokenRefreshMinInterval - time.Second)
	mapping := store.Mappings["u1"]
	mapping.BackendAccount.BackendToken = "backend-token"
	mapping.BackendAccount.BackendUserID = "backend-user"
	mapping.BackendAccount.TokenUpdatedAt = &oldTokenTime
	store.Mappings["u1"] = mapping

	req := mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/item-1/PlaybackInfo", nil)
	req.Header.Set("X-Emby-Token", gatewayToken)
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("proxy status = %d, want 401: %s", resp.StatusCode, string(body))
	}
	if refreshCount != 1 || playbackCount != 1 {
		t.Fatalf("refresh/playback counts = %d/%d, want 1/1", refreshCount, playbackCount)
	}
}

func TestBackendLoginFailureBackoff(t *testing.T) {
	var authCount int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/emby/Users/AuthenticateByName" {
			authCount++
			http.Error(w, "backend auth failed", http.StatusUnauthorized)
			return
		}
		t.Fatalf("unexpected backend request %s %s", r.Method, r.URL.String())
	}))
	defer backend.Close()

	store := testStore(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", GatewayServerID: "gateway-server"}, store))
	defer gw.Close()

	loginResp := do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Pw":"alice-pass"}`))
	var login map[string]any
	decodeJSON(t, loginResp.Body, &login)
	_ = loginResp.Body.Close()
	token, _ := login["AccessToken"].(string)

	for i := 0; i < 2; i++ {
		req := mustRequest(t, http.MethodGet, gw.URL+"/emby/System/Info", nil)
		req.Header.Set("X-Emby-Token", token)
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("request %d status = %d, want 502", i+1, resp.StatusCode)
		}
	}
	if authCount != 1 {
		t.Fatalf("backend auth count = %d, want 1 due to cooldown", authCount)
	}
}

func TestProxyRetryRewritesBodyWithRefreshedToken(t *testing.T) {
	const (
		backendUserID   = "backend-user"
		syntheticUserID = "gateway-user"
	)
	var refreshCount int
	var logoutCount int
	var postCount int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/emby/Users/AuthenticateByName":
			refreshCount++
			writeTestJSON(w, map[string]any{
				"AccessToken": "backend-token-2",
				"ServerId":    "backend-server",
				"User": map[string]any{
					"Id":   backendUserID,
					"Name": "shared",
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/emby/Users/"+backendUserID+"/Items":
			postCount++
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			body := string(data)
			wantToken := "backend-token-" + strconv.Itoa(postCount)
			if r.Header.Get("X-Emby-Token") != wantToken || !strings.Contains(body, wantToken) {
				t.Fatalf("request %d did not use %s in header/body: header=%q body=%s", postCount, wantToken, r.Header.Get("X-Emby-Token"), body)
			}
			if postCount == 1 {
				http.Error(w, "stale", http.StatusUnauthorized)
				return
			}
			if strings.Contains(body, "backend-token-1") {
				t.Fatalf("retry body still contained stale token: %s", body)
			}
			writeTestJSON(w, map[string]any{"ok": true})
		case r.Method == http.MethodGet && r.URL.Path == "/emby/System/Info":
			if r.Header.Get("X-Emby-Token") == "backend-token-1" {
				http.Error(w, "stale", http.StatusUnauthorized)
				return
			}
			writeTestJSON(w, map[string]any{"Id": "backend-server"})
		case r.Method == http.MethodPost && r.URL.Path == "/emby/Sessions/Logout":
			logoutCount++
			if r.Header.Get("X-Emby-Token") != "backend-token-1" {
				t.Fatalf("logout token = %q, want old backend token", r.Header.Get("X-Emby-Token"))
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected backend request %s %s", r.Method, r.URL.String())
		}
	}))
	defer backend.Close()

	store := testStore(backend.URL + "/emby")
	store.Users["u1"] = MemoryUser{GatewayUser: GatewayUser{ID: "u1", Username: "alice", SyntheticUserID: syntheticUserID, Enabled: true}, Password: "alice-pass"}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", GatewayServerID: "gateway-server"}, store))
	defer gw.Close()

	loginResp := do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Pw":"alice-pass"}`))
	var login map[string]any
	decodeJSON(t, loginResp.Body, &login)
	_ = loginResp.Body.Close()
	gatewayToken, _ := login["AccessToken"].(string)
	oldTokenTime := time.Now().UTC().Add(-backendTokenRefreshMinInterval - time.Second)
	mapping := store.Mappings["u1"]
	mapping.BackendAccount.BackendToken = "backend-token-1"
	mapping.BackendAccount.BackendUserID = backendUserID
	mapping.BackendAccount.TokenUpdatedAt = &oldTokenTime
	store.Mappings["u1"] = mapping

	body := `{"Token":"` + gatewayToken + `","UserId":"` + syntheticUserID + `"}`
	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Users/"+syntheticUserID+"/Items", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Token", gatewayToken)
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("proxy status = %d: %s", resp.StatusCode, string(data))
	}
	if refreshCount != 1 || postCount != 2 {
		t.Fatalf("refresh/post counts = %d/%d, want 1/2", refreshCount, postCount)
	}
	if logoutCount != 1 {
		t.Fatalf("backend logout count = %d, want 1", logoutCount)
	}
}

func TestRefreshBackendSessionReusesRotatedToken(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("refresh should not call backend when token already rotated: %s %s", r.Method, r.URL.String())
	}))
	defer backend.Close()

	store := testStore(backend.URL + "/emby")
	mapping := store.Mappings["u1"]
	mapping.BackendAccount.BackendToken = "new-token"
	mapping.BackendAccount.BackendUserID = "backend-user"
	store.Mappings["u1"] = mapping
	session := testSession(backend.URL + "/emby")
	session.BackendToken = "old-token"
	session.BackendUserID = "backend-user"
	session.BackendAccount = mapping.BackendAccount

	server := NewServer(Config{GatewayBasePath: "/emby"}, store)
	if err := server.refreshBackendSession(context.Background(), session, "old-token"); err != nil {
		t.Fatalf("refresh backend session: %v", err)
	}
	if session.BackendToken != "new-token" {
		t.Fatalf("session token = %q, want rotated token", session.BackendToken)
	}
}

func TestBackendLoginIgnoresCallerCancellation(t *testing.T) {
	backend := testAuthBackend(t)
	defer backend.Close()
	store := testStore(backend.URL + "/emby")
	session := testSession(backend.URL + "/emby")
	session.BackendToken = ""
	session.BackendUserID = ""
	session.BackendAccount = store.Mappings["u1"].BackendAccount

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	server := NewServer(Config{GatewayBasePath: "/emby"}, store)
	if err := server.ensureBackendSession(ctx, session); err != nil {
		t.Fatalf("ensure backend session with canceled caller context: %v", err)
	}
	if session.BackendToken == "" || store.Mappings["u1"].BackendAccount.BackendToken == "" {
		t.Fatalf("backend token was not stored after canceled caller context")
	}
}

func TestDisabledBackendAccountDoesNotLogin(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("disabled backend account should not call backend: %s %s", r.Method, r.URL.String())
	}))
	defer backend.Close()
	store := testStore(backend.URL + "/emby")
	mapping := store.Mappings["u1"]
	mapping.BackendAccount.Enabled = false
	store.Mappings["u1"] = mapping
	session := testSession(backend.URL + "/emby")
	session.BackendToken = ""
	session.BackendUserID = ""
	session.BackendAccount = mapping.BackendAccount

	server := NewServer(Config{GatewayBasePath: "/emby"}, store)
	if err := server.ensureBackendSession(context.Background(), session); !errors.Is(err, ErrDisabled) {
		t.Fatalf("ensure backend session error = %v, want ErrDisabled", err)
	}
}

func TestAnonymousPublicInfoAndPing(t *testing.T) {
	store := NewMemoryStore()
	store.Mappings["m1"] = UserMapping{BackendAccount: BackendAccount{ID: "b1", ServerID: "s1", Enabled: true, Server: EmbyServer{ID: "s1", Enabled: true, ServerVersion: "4.9.1"}}}
	store.Mappings["m2"] = UserMapping{BackendAccount: BackendAccount{ID: "b2", ServerID: "s2", Enabled: true, Server: EmbyServer{ID: "s2", Enabled: true, ServerVersion: "4.10.0"}}}
	gw := httptest.NewServer(NewServer(Config{
		PublicBaseURL:   "https://media.example.com",
		GatewayBasePath: "/emby",
		GatewayServerID: "gateway-server-id",
	}, store))
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
	if info["Version"] != "4.10.0" {
		t.Fatalf("public info Version = %#v, want highest backend version 4.10.0", info["Version"])
	}

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		pingResp := do(t, mustRequest(t, method, gw.URL+"/emby/System/Ping", nil))
		_ = pingResp.Body.Close()
		if pingResp.StatusCode != http.StatusOK {
			t.Fatalf("%s ping status %d", method, pingResp.StatusCode)
		}
	}
}

func TestPublicSystemInfoProbesBackendWhenVersionMissing(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/emby/System/Info/Public" {
			t.Fatalf("unexpected backend request %s %s", r.Method, r.URL.String())
		}
		writeTestJSON(w, map[string]any{"Id": "real-server", "ServerName": "Real Emby", "Version": "4.9.5.0"})
	}))
	defer backend.Close()
	store := NewMemoryStore()
	store.Mappings["m1"] = UserMapping{BackendAccount: BackendAccount{ID: "b1", ServerID: "s1", BaseURL: backend.URL + "/emby", Enabled: true, Server: EmbyServer{ID: "s1", BaseURL: backend.URL + "/emby", Enabled: true}}}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", GatewayServerID: "gateway-server"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/System/Info/Public", nil))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("public info status = %d: %s", resp.StatusCode, string(body))
	}
	var info map[string]any
	decodeJSON(t, resp.Body, &info)
	if info["Version"] != "4.9.5.0" {
		t.Fatalf("public info Version = %#v, want probed backend version", info["Version"])
	}
}

func TestPublicSystemInfoFallsBackWithoutBackendVersion(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/emby/System/Info/Public" {
			t.Fatalf("unexpected backend request %s %s", r.Method, r.URL.String())
		}
		writeTestJSON(w, map[string]any{"Id": "real-server", "ServerName": "Real Emby"})
	}))
	defer backend.Close()
	store := NewMemoryStore()
	store.Mappings["m1"] = UserMapping{BackendAccount: BackendAccount{ID: "b1", ServerID: "s1", BaseURL: backend.URL + "/emby", Enabled: true, Server: EmbyServer{ID: "s1", BaseURL: backend.URL + "/emby", Enabled: true}}}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", GatewayServerID: "gateway-server"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/System/Info/Public", nil))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("public info status = %d, want 200: %s", resp.StatusCode, string(body))
	}
	var info map[string]any
	decodeJSON(t, resp.Body, &info)
	if info["Version"] != defaultBackendServerVersion {
		t.Fatalf("public info Version = %#v, want fallback %s", info["Version"], defaultBackendServerVersion)
	}
}

func TestCompareVersionsPrefersReleaseOverPrerelease(t *testing.T) {
	if compareVersions("4.9.0.30", "4.9.0.30-beta") <= 0 {
		t.Fatal("release version should compare greater than matching prerelease")
	}
	if compareVersions("4.9.0.30", "4.9.0.30-beta.2") <= 0 {
		t.Fatal("release version should compare greater than matching prerelease with numeric suffix")
	}
	if compareVersions("4.10.0", "4.9.9") <= 0 {
		t.Fatal("numeric version comparison should compare each segment")
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

		loginResp := do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Pw":"alice-pass"}`))
		if loginResp.StatusCode != http.StatusOK {
			_ = loginResp.Body.Close()
			t.Fatalf("login status = %d, want 200", loginResp.StatusCode)
		}
		var login map[string]any
		decodeJSON(t, loginResp.Body, &login)
		_ = loginResp.Body.Close()
		token, _ := login["AccessToken"].(string)

		req := mustRequest(t, http.MethodGet, gw.URL+"/emby/System/Info", nil)
		req.Header.Set("X-Emby-Token", token)
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("proxy status = %d, want 502", resp.StatusCode)
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

func TestBrandingShimsAnonymousExactResponses(t *testing.T) {
	var backendHits int
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		backendHits++
		t.Fatal("branding shim reached backend")
	}))
	defer backend.Close()

	store := testStore(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	t.Run("configuration", func(t *testing.T) {
		resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Branding/Configuration?foo=1&api_key=%ZZ", nil))
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if string(body) != "{}" {
			t.Fatalf("body = %q, want exact {}", body)
		}
		if resp.Header.Get("Content-Type") != "application/json; charset=utf-8" {
			t.Fatalf("Content-Type = %q", resp.Header.Get("Content-Type"))
		}
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
		}
	})

	t.Run("css", func(t *testing.T) {
		resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Branding/Css.css?token=ignored", nil))
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if len(body) != 0 {
			t.Fatalf("body len = %d, want 0", len(body))
		}
		if resp.Header.Get("Content-Type") != "text/css; charset=utf-8" {
			t.Fatalf("Content-Type = %q", resp.Header.Get("Content-Type"))
		}
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
		}
	})

	if backendHits != 0 {
		t.Fatalf("backend hits = %d", backendHits)
	}
}

func TestBrandingShimsPathPolicyDenial(t *testing.T) {
	store := NewMemoryStore()
	store.PathPolicies = []PathPolicy{
		{Method: http.MethodGet, Path: "/Branding/Configuration", Action: "deny", Enabled: true},
		{Method: http.MethodGet, Path: "/Branding/Css.css", Action: "deny", Enabled: true},
	}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	for _, path := range []string{"/emby/Branding/Configuration", "/emby/Branding/Css.css"} {
		resp := do(t, mustRequest(t, http.MethodGet, gw.URL+path, nil))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s status = %d, want 403", path, resp.StatusCode)
		}
	}
}

func TestBrandingNonGETAndOtherPathsRemainAuthenticated(t *testing.T) {
	var backendHits int
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		backendHits++
	}))
	defer backend.Close()

	store := testStore(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/emby/Branding/Configuration"},
		{http.MethodHead, "/emby/Branding/Configuration"},
		{http.MethodPost, "/emby/Branding/Css.css"},
		{http.MethodGet, "/emby/Branding/Other"},
		{http.MethodGet, "/emby/Branding/Css"},
	} {
		resp := do(t, mustRequest(t, tc.method, gw.URL+tc.path, nil))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d, want 401", tc.method, tc.path, resp.StatusCode)
		}
	}
	if backendHits != 0 {
		t.Fatalf("backend hits = %d", backendHits)
	}
}

func TestPlaybackEventsAndStateAreRecordedWithoutForwarding(t *testing.T) {
	const gatewayToken = "gateway-token"
	var forwarded []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		forwarded = append(forwarded, r.URL.Path+":"+string(body))
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
		{"/Sessions/Playing/Stopped", `{"Item":{"Id":"item-1","RunTimeTicks":1000},"UserId":"gateway-user","PositionTicks":950}`},
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
	if len(forwarded) != 0 || len(store.PlaybackEvents) != 3 {
		t.Fatalf("forwarded=%d events=%d, want 0/3", len(forwarded), len(store.PlaybackEvents))
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "item-1")
	if err != nil {
		t.Fatalf("find playback state: %v", err)
	}
	if !state.Played || state.PlayCount != 1 || state.PlaybackPositionTicks != 0 || state.PlayedPercentage == nil || *state.PlayedPercentage != 100 || state.LastPlayedDate == nil || state.RunTimeTicks != 1000 {
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

func TestPlaybackReportsSucceedWhenBackendUnavailable(t *testing.T) {
	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession("http://127.0.0.1:1/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Progress?api_key=gateway-token&ItemId=item-1&PlaybackPositionTicks=700", nil)
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("playback status = %d, want 204", resp.StatusCode)
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "item-1")
	if err != nil || state.PlaybackPositionTicks != 700 {
		t.Fatalf("playback state = %#v err=%v", state, err)
	}
}

func TestPlaybackPingAndCapabilitiesAreLocalOnly(t *testing.T) {
	var forwarded []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = append(forwarded, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	for _, path := range []string{"/Sessions/Playing/Ping", "/Sessions/Capabilities", "/Sessions/Capabilities/Full"} {
		req := mustRequest(t, http.MethodPost, gw.URL+"/emby"+path+"?api_key=gateway-token", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s status = %d, want 204", path, resp.StatusCode)
		}
	}
	if len(forwarded) != 0 || len(store.PlaybackEvents) != 0 || len(store.PlaybackStates) != 0 {
		t.Fatalf("forwarded=%v events=%d states=%d, want no local/proxy side effects", forwarded, len(store.PlaybackEvents), len(store.PlaybackStates))
	}
}

func TestRemoteControlPlaybackRequestStillForwards(t *testing.T) {
	var forwarded []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = append(forwarded, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/session-1/Playing/Pause?api_key=gateway-token", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("remote control status = %d, want 204", resp.StatusCode)
	}
	if len(forwarded) != 1 || forwarded[0] != "POST /emby/Sessions/session-1/Playing/Pause" {
		t.Fatalf("forwarded = %v, want remote control request", forwarded)
	}
}

func TestStoppedPlaybackUsesEmbyTicksForResumeThresholds(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	state := &PlaybackState{
		GatewayUserID:         "u1",
		SyntheticUserID:       "gateway-user",
		ItemID:                "movie-1",
		RunTimeTicks:          30 * 60 * embyTicksPerSecond,
		PlaybackPositionTicks: 3 * 60 * embyTicksPerSecond,
	}
	applyStoppedPlaybackState(state, now, false, resumePolicy{MinPct: defaultMinResumePct, MaxPct: defaultMaxResumePct, MinDurationSeconds: defaultMinResumeDurationSeconds})
	if state.Played || state.PlaybackPositionTicks != 3*60*embyTicksPerSecond || state.PlayCount != 0 || state.LastPlayedDate != nil {
		t.Fatalf("10%% into a 30 minute video should remain resumable: %#v", state)
	}

	unknownRuntime := &PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "movie-2", PlaybackPositionTicks: 1000}
	applyStoppedPlaybackState(unknownRuntime, now, false, resumePolicy{MinPct: defaultMinResumePct, MaxPct: defaultMaxResumePct, MinDurationSeconds: defaultMinResumeDurationSeconds})
	if unknownRuntime.Played || unknownRuntime.PlaybackPositionTicks != 1000 || unknownRuntime.PlayCount != 0 {
		t.Fatalf("unknown runtime should keep resume position instead of completing: %#v", unknownRuntime)
	}
}

func TestPlaybackDetailsParsesNumericStrings(t *testing.T) {
	details, ok := playbackDetailsFromJSON(map[string]any{
		"ItemId":                "item-1",
		"PlaybackPositionTicks": "12345",
		"RunTimeTicks":          "100000",
		"PlayedPercentage":      "12.5",
		"Item":                  map[string]any{"Id": "item-1", "RunTimeTicks": "100000"},
	})
	if !ok || !details.HasPositionTicks || details.PositionTicks != 12345 || !details.HasRunTimeTicks || details.RunTimeTicks != 100000 || details.PlayedPercentage == nil || *details.PlayedPercentage != 12.5 {
		t.Fatalf("numeric strings were not parsed: %#v ok=%v", details, ok)
	}
}

func TestUserDataVirtualizationIsGatewayUserScoped(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"Item": map[string]any{
				"Id":           "item-1",
				"RunTimeTicks": float64(10000),
				"UserData":     map[string]any{"Played": true, "PlaybackPositionTicks": float64(9999), "PlayedPercentage": float64(99), "PlayCount": float64(9)},
			},
			"Items": []any{
				map[string]any{"Id": "item-1", "RunTimeTicks": float64(10000), "UserData": map[string]any{"Played": true, "PlaybackPositionTicks": float64(9999), "PlayedPercentage": float64(99), "PlayCount": float64(9)}},
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
	pct2 := 88.25
	lastPlayed := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", ItemID: "item-1", PlaybackPositionTicks: 4200, Played: false, PlayCount: 1})
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u2", ItemID: "item-1", PlaybackPositionTicks: 8800, PlayedPercentage: &pct2, Played: true, LastPlayedDate: &lastPlayed, PlayCount: 3})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	u1 := fetchUserData(t, gw.URL+"/emby/Items/item-1?api_key=token-u1")
	u2 := fetchUserData(t, gw.URL+"/emby/Items/item-1?api_key=token-u2")
	u3 := fetchUserData(t, gw.URL+"/emby/Items/item-1?api_key=token-u3")
	if u1["Played"] != false || int(u1["PlaybackPositionTicks"].(float64)) != 4200 || u1["PlayedPercentage"].(float64) != 42 || int(u1["PlayCount"].(float64)) != 1 {
		t.Fatalf("unexpected u1 user data: %#v", u1)
	}
	if u2["Played"] != true || int(u2["PlaybackPositionTicks"].(float64)) != 8800 || u2["PlayedPercentage"].(float64) != 100 || int(u2["PlayCount"].(float64)) != 3 || u2["LastPlayedDate"] == "" {
		t.Fatalf("unexpected u2 user data: %#v", u2)
	}
	if u3["Played"] != false || int(u3["PlaybackPositionTicks"].(float64)) != 0 || u3["PlayedPercentage"] != nil || int(u3["PlayCount"].(float64)) != 0 || u3["LastPlayedDate"] != nil {
		t.Fatalf("missing state should not leak backend user data: %#v", u3)
	}
}

func TestCompressedJSONUserDataIsVirtualized(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Fatalf("backend did not receive gzip-capable request")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		_ = json.NewEncoder(gz).Encode(map[string]any{
			"Items": []any{
				map[string]any{"Id": "episode-1", "Name": "Episode 1", "Type": "Episode", "UserData": map[string]any{"Played": true, "PlaybackPositionTicks": float64(9999), "PlayedPercentage": float64(99), "PlayCount": float64(9), "IsFavorite": false, "UnplayedItemCount": float64(12)}},
			},
		})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "episode-1", IsFavorite: true})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	req := mustRequest(t, http.MethodGet, gw.URL+"/emby/Shows/show-1/Episodes?api_key=gateway-token", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") == "gzip" {
		t.Fatal("gateway returned compressed JSON before rewriting")
	}
	var body map[string]any
	decodeJSON(t, resp.Body, &body)
	items := body["Items"].([]any)
	userData := items[0].(map[string]any)["UserData"].(map[string]any)
	if userData["Played"] != false || int(userData["PlaybackPositionTicks"].(float64)) != 0 || userData["PlayedPercentage"] != nil || int(userData["PlayCount"].(float64)) != 0 || userData["IsFavorite"] != true || userData["UnplayedItemCount"] != nil {
		t.Fatalf("compressed JSON UserData was not virtualized: %#v", userData)
	}
}

func TestProxyUserDataOverlayUsesBatchLookup(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"Items": []any{
				map[string]any{"Id": "episode-1", "Name": "Episode 1", "Type": "Episode", "UserData": map[string]any{}},
				map[string]any{"Id": "episode-2", "Name": "Episode 2", "Type": "Episode", "UserData": map[string]any{}},
				map[string]any{"Id": "episode-1", "Name": "Episode 1 duplicate", "Type": "Episode", "UserData": map[string]any{}},
			},
		})
	}))
	defer backend.Close()

	base := NewMemoryStore()
	store := &countingPlaybackStore{MemoryStore: base}
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "episode-1", PlaybackPositionTicks: 1200})
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "episode-2", IsFavorite: true})
	gateway := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gateway.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gateway.URL+"/emby/Shows/show-1/Episodes?api_key=gateway-token", nil))
	defer resp.Body.Close()
	var body map[string]any
	decodeJSON(t, resp.Body, &body)
	items := body["Items"].([]any)
	firstUserData := items[0].(map[string]any)["UserData"].(map[string]any)
	secondUserData := items[1].(map[string]any)["UserData"].(map[string]any)
	if int(firstUserData["PlaybackPositionTicks"].(float64)) != 1200 || secondUserData["IsFavorite"] != true {
		t.Fatalf("UserData was not overlaid from batched states: first=%#v second=%#v", firstUserData, secondUserData)
	}
	if store.singleLookups != 0 {
		t.Fatalf("FindPlaybackState calls = %d, want 0", store.singleLookups)
	}
	if store.batchLookups != 1 {
		t.Fatalf("ListPlaybackStatesByItemIDs calls = %d, want 1", store.batchLookups)
	}
	if got := strings.Join(store.batchItemIDs, ","); got != "episode-1,episode-2" {
		t.Fatalf("batched item ids = %q, want episode-1,episode-2", got)
	}
}

func TestProxyUserDataOverlayIgnoresOrphanedState(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		item := map[string]any{"Id": "episode-1", "Name": "Episode 1", "Type": "Episode", "UserData": map[string]any{"Played": true, "PlaybackPositionTicks": float64(9999), "PlayedPercentage": float64(99)}}
		writeTestJSON(w, map[string]any{"Item": item, "Items": []any{item}})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	orphanedAt := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "episode-1", PlaybackPositionTicks: 1234, PlayedPercentage: floatPtr(12), OrphanedAt: &orphanedAt})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	userData := fetchUserData(t, gw.URL+"/emby/Items/episode-1?api_key=gateway-token")
	if userData["Played"] != false || int(userData["PlaybackPositionTicks"].(float64)) != 0 || userData["PlayedPercentage"] != nil {
		t.Fatalf("orphaned state should not overlay backend data: %#v", userData)
	}
}

func TestResumeUsesGatewayStateAndResolvesExistingItems(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Users/backend-user/Items" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		if r.URL.Query().Get("Ids") != "item-u1,missing-item,item-u1-older" {
			t.Fatalf("backend Ids = %q, want user scoped resume ids", r.URL.Query().Get("Ids"))
		}
		writeTestJSON(w, map[string]any{"Items": []any{
			map[string]any{"Id": "item-u1", "Name": "Episode 2", "Type": "Episode", "SeriesId": "show-1", "UserData": map[string]any{"PlaybackPositionTicks": float64(999)}},
			map[string]any{"Id": "item-u1-older", "Name": "Episode 1", "Type": "Episode", "SeriesId": "show-1", "UserData": map[string]any{"PlaybackPositionTicks": float64(999)}},
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
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "item-u1", SeriesID: "show-1", PlaybackPositionTicks: 1200, PlayedPercentage: &pct, Fingerprint: "type=Episode", UpdatedAt: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)})
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "item-u1-older", SeriesID: "show-1", PlaybackPositionTicks: 1000, UpdatedAt: time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)})
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
	resolved, err := store.FindPlaybackState(context.Background(), "u1", "item-u1")
	if err != nil || resolved.OrphanedAt != nil || resolved.Fingerprint == "type=Episode" {
		t.Fatalf("partial fingerprint should be compatible with resolved item metadata: %#v err=%v", resolved, err)
	}
}

func TestResumeMediaTypeFilterDoesNotOrphanFilteredItems(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("MediaTypes") != "" {
			t.Fatalf("resolution request should not forward MediaTypes filter: %s", r.URL.RawQuery)
		}
		writeTestJSON(w, map[string]any{"Items": []any{
			map[string]any{"Id": "video-1", "Name": "Video", "Type": "Movie", "MediaType": "Video", "UserData": map[string]any{}},
			map[string]any{"Id": "audio-1", "Name": "Audio", "Type": "Audio", "MediaType": "Audio", "UserData": map[string]any{}},
		}})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "video-1", PlaybackPositionTicks: 1200})
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "audio-1", PlaybackPositionTicks: 2200})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items/Resume?api_key=gateway-token&MediaTypes=Video", nil))
	defer resp.Body.Close()
	var body map[string]any
	decodeJSON(t, resp.Body, &body)
	items := body["Items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["Id"] != "video-1" {
		t.Fatalf("resume items = %#v, want only video-1", items)
	}
	audio, err := store.FindPlaybackState(context.Background(), "u1", "audio-1")
	if err != nil || audio.OrphanedAt != nil {
		t.Fatalf("filtered audio item should not be orphaned: %#v err=%v", audio, err)
	}
}

func TestResumeRepairsPreviouslyOrphanedState(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Users/backend-user/Items" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		writeTestJSON(w, map[string]any{"Items": []any{
			map[string]any{"Id": "episode-1", "Name": "Episode 1", "Type": "Episode", "MediaType": "Video", "UserData": map[string]any{}},
		}})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	orphanedAt := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "episode-1", PlaybackPositionTicks: 1200, OrphanedAt: &orphanedAt})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items/Resume?api_key=gateway-token&MediaTypes=Video", nil))
	defer resp.Body.Close()
	var body map[string]any
	decodeJSON(t, resp.Body, &body)
	items := body["Items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["Id"] != "episode-1" {
		t.Fatalf("resume items = %#v, want repaired episode-1", items)
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "episode-1")
	if err != nil || state.OrphanedAt != nil || state.LastSeenAt == nil {
		t.Fatalf("orphaned state was not repaired: %#v err=%v", state, err)
	}
}

func TestResumeResolutionIgnoresCollectionFiltersForOrphanDecision(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ParentId") != "" || r.URL.Query().Get("IsPlayed") != "" {
			t.Fatalf("resolution request should not forward collection filters: %s", r.URL.RawQuery)
		}
		writeTestJSON(w, map[string]any{"Items": []any{
			map[string]any{"Id": "movie-1", "Name": "Movie", "Type": "Movie", "MediaType": "Video", "UserData": map[string]any{}},
		}})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "movie-1", PlaybackPositionTicks: 1200})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items/Resume?api_key=gateway-token&ParentId=wrong-parent&IsPlayed=false", nil))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resume status = %d", resp.StatusCode)
	}
	var body map[string]any
	decodeJSON(t, resp.Body, &body)
	if len(body["Items"].([]any)) != 0 {
		t.Fatalf("parent filter should exclude item without orphaning it: %#v", body)
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "movie-1")
	if err != nil || state.OrphanedAt != nil {
		t.Fatalf("collection filter should not orphan existing state: %#v err=%v", state, err)
	}
}

func TestResumeDoesNotOrphanItemsBeyondResolutionBatchLimit(t *testing.T) {
	var requestSizes []int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids := splitFilterValues([]string{r.URL.Query().Get("Ids")})
		if len(ids) == 0 || len(ids) > personalIDBatchLimit {
			t.Fatalf("resolved ids = %d, want 1..%d", len(ids), personalIDBatchLimit)
		}
		requestSizes = append(requestSizes, len(ids))
		items := make([]any, 0, len(ids))
		for _, id := range ids {
			items = append(items, map[string]any{"Id": id, "Name": id, "Type": "Movie", "MediaType": "Video", "UserData": map[string]any{}})
		}
		writeTestJSON(w, map[string]any{"Items": items})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	for i := 0; i < personalIDBatchLimit+1; i++ {
		_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "item-" + strconv.Itoa(i), PlaybackPositionTicks: 1000, UpdatedAt: now.Add(-time.Duration(i) * time.Minute)})
	}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items/Resume?api_key=gateway-token", nil))
	_ = resp.Body.Close()
	state, err := store.FindPlaybackState(context.Background(), "u1", "item-200")
	if err != nil || state.OrphanedAt != nil {
		t.Fatalf("unrequested item should not be orphaned: %#v err=%v", state, err)
	}
	if len(requestSizes) != 2 || requestSizes[0] != personalIDBatchLimit || requestSizes[1] != 1 {
		t.Fatalf("batch sizes = %v, want [200 1]", requestSizes)
	}
}

func TestSeriesAndSeasonUserDataAreAggregatedFromGatewayState(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"Items": []any{
			map[string]any{"Id": "show-1", "Name": "Show", "Type": "Series", "RecursiveItemCount": float64(3), "UserData": map[string]any{"Played": true, "UnplayedItemCount": float64(0), "PlayedPercentage": float64(100)}},
			map[string]any{"Id": "season-1", "Name": "Season 1", "Type": "Season", "ChildCount": float64(2), "UserData": map[string]any{"Played": true, "UnplayedItemCount": float64(0), "PlayedPercentage": float64(100)}},
		}})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	lastPlayed := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "ep-1", SeriesID: "show-1", SeasonID: "season-1", Played: true, LastPlayedDate: &lastPlayed})
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "ep-2", SeriesID: "show-1", SeasonID: "season-1", Played: false})
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "ep-3", SeriesID: "show-1", SeasonID: "season-2", Played: true})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items?api_key=gateway-token", nil))
	defer resp.Body.Close()
	var body map[string]any
	decodeJSON(t, resp.Body, &body)
	items := body["Items"].([]any)
	seriesData := items[0].(map[string]any)["UserData"].(map[string]any)
	seasonData := items[1].(map[string]any)["UserData"].(map[string]any)
	if seriesData["Played"] != false || int(seriesData["UnplayedItemCount"].(float64)) != 1 || int(seriesData["PlayedPercentage"].(float64)) != 66 {
		t.Fatalf("unexpected series data: %#v", seriesData)
	}
	if seasonData["Played"] != false || int(seasonData["UnplayedItemCount"].(float64)) != 1 || int(seasonData["PlayedPercentage"].(float64)) != 50 || seasonData["LastPlayedDate"] == nil {
		t.Fatalf("unexpected season data: %#v", seasonData)
	}
}

func TestPersonalStateWritesAreTerminatedAtGateway(t *testing.T) {
	var writeRequests int
	var metadataRequests int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/emby/Users/backend-user/Items/item-1" {
			metadataRequests++
			writeTestJSON(w, map[string]any{"Id": "item-1", "Name": "Episode 1", "Type": "Episode", "SeriesId": "show-1", "SeasonId": "season-1", "RunTimeTicks": float64(10000)})
			return
		}
		writeRequests++
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
	if !state.Played || !state.IsFavorite || state.Likes == nil || !*state.Likes || state.PlaybackPositionTicks != 321 || state.SeriesID != "show-1" || state.SeasonID != "season-1" || state.RunTimeTicks != 10000 {
		t.Fatalf("personal state not persisted: %#v", state)
	}
	if writeRequests != 0 || metadataRequests != len(requests) {
		t.Fatalf("writeRequests=%d metadataRequests=%d, want 0/%d", writeRequests, metadataRequests, len(requests))
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
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "resume-1", PlaybackPositionTicks: 1000})
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

	sawIDs = ""
	sawFilters = ""
	resp = do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items?api_key=gateway-token&IsFavorite=true", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("boolean favorite items status = %d", resp.StatusCode)
	}
	if sawIDs != "fav-1" || sawFilters != "" {
		t.Fatalf("boolean backend query Ids=%q Filters=%q, want fav-1/no personal filter", sawIDs, sawFilters)
	}

	sawIDs = ""
	sawFilters = ""
	resp = do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items?api_key=gateway-token&IsResumable=true", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("boolean resumable items status = %d", resp.StatusCode)
	}
	if sawIDs != "resume-1" || sawFilters != "" {
		t.Fatalf("resumable backend query Ids=%q Filters=%q, want resume-1/no personal filter", sawIDs, sawFilters)
	}
}

func TestPersonalFiltersApplyToNonUserItemLists(t *testing.T) {
	var sawIDs string
	var sawFilters string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Users/backend-user/Items" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		sawIDs = r.URL.Query().Get("Ids")
		sawFilters = r.URL.Query().Get("Filters")
		writeTestJSON(w, map[string]any{"Items": []any{map[string]any{"Id": sawIDs, "Type": "Episode", "SeriesId": "show-1", "UserData": map[string]any{}}}, "TotalRecordCount": 1})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "ep-played", Played: true})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Shows/show-1/Episodes?api_key=gateway-token&UserId=gateway-user&Filters=IsPlayed", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filtered episodes status = %d", resp.StatusCode)
	}
	if sawIDs != "ep-played" || sawFilters != "" {
		t.Fatalf("backend query Ids=%q Filters=%q, want ep-played/no personal filter", sawIDs, sawFilters)
	}
}

func TestPositivePersonalFilterResolvesAllIDsWithoutCap(t *testing.T) {
	var requestSizes []int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Users/backend-user/Items" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		ids := splitFilterValues([]string{r.URL.Query().Get("Ids")})
		requestSizes = append(requestSizes, len(ids))
		items := make([]any, 0, len(ids))
		for _, id := range ids {
			items = append(items, map[string]any{"Id": id, "Name": id, "Type": "Movie", "UserData": map[string]any{}})
		}
		writeTestJSON(w, map[string]any{"Items": items, "TotalRecordCount": len(items)})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	for i := 0; i < personalIDBatchLimit+1; i++ {
		_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "fav-" + strconv.Itoa(i), ItemType: "Movie", IsFavorite: true})
	}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items?api_key=gateway-token&IsFavorite=true", nil))
	defer resp.Body.Close()
	var body map[string]any
	decodeJSON(t, resp.Body, &body)
	if got := int(body["TotalRecordCount"].(float64)); got != personalIDBatchLimit+1 {
		t.Fatalf("TotalRecordCount = %d, want %d", got, personalIDBatchLimit+1)
	}
	if len(requestSizes) != 2 || requestSizes[0] != personalIDBatchLimit || requestSizes[1] != 1 {
		t.Fatalf("request sizes = %v, want [%d 1]", requestSizes, personalIDBatchLimit)
	}
}

func TestAggregatePersonalFilterEndpointIsRejected(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("aggregate personal filter should not reach backend: %s", r.URL.String())
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "fav-1", ItemType: "Movie", IsFavorite: true})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Artists?api_key=gateway-token&Filters=IsFavorite", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("filtered artist status = %d, want 400", resp.StatusCode)
	}
}

func TestPositivePersonalFilterKeepsBackendOnlyFilters(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids := splitFilterValues([]string{r.URL.Query().Get("Ids")})
		items := make([]any, 0, len(ids))
		for _, id := range ids {
			if r.URL.Query().Get("SearchTerm") == "foo" && id != "fav-foo" {
				continue
			}
			items = append(items, map[string]any{"Id": id, "Name": id, "Type": "Movie", "UserData": map[string]any{}})
		}
		writeTestJSON(w, map[string]any{"Items": items, "TotalRecordCount": len(items)})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "fav-foo", ItemType: "Movie", IsFavorite: true})
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "fav-bar", ItemType: "Movie", IsFavorite: true})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items?api_key=gateway-token&IsFavorite=true&SearchTerm=foo", nil))
	defer resp.Body.Close()
	var body map[string]any
	decodeJSON(t, resp.Body, &body)
	ids := itemIDsFromResponse(body["Items"].([]any))
	if strings.Join(ids, ",") != "fav-foo" || int(body["TotalRecordCount"].(float64)) != 1 {
		t.Fatalf("filtered favorite ids=%v body=%#v", ids, body)
	}
}

func TestPositivePersonalFilterBackendFailureReturnsBadGateway(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "backend failed", http.StatusInternalServerError)
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "fav-1", IsFavorite: true})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items?api_key=gateway-token&IsFavorite=true", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("positive filter backend failure status = %d, want 502", resp.StatusCode)
	}
}

func TestClearlyNonItemPersonalFilterPathIsPassedThrough(t *testing.T) {
	var reached bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if r.URL.Path != "/emby/System/Info" || r.URL.Query().Get("IsFavorite") != "true" {
			t.Fatalf("unexpected backend request: %s", r.URL.String())
		}
		writeTestJSON(w, map[string]any{"Id": "backend-server"})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/System/Info?api_key=gateway-token&IsFavorite=true", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !reached {
		t.Fatalf("non-item personal filter passthrough status=%d reached=%v", resp.StatusCode, reached)
	}
}

func TestUnknownPersonalFilterPathIsRejected(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unsupported personal filter should not reach backend: %s", r.URL.String())
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/UnknownList?api_key=gateway-token&IsFavorite=true", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unsupported personal filter status = %d, want 400", resp.StatusCode)
	}

	resp = do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Views?api_key=gateway-token&Filters=IsFavorite", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unsupported user personal filter status = %d, want 400", resp.StatusCode)
	}
}

func TestNegativePersonalFilterBackfillsFromUpstreamPages(t *testing.T) {
	items := make([]map[string]any, 10)
	for i := range items {
		items[i] = map[string]any{"Id": "item-" + strconv.Itoa(i), "Name": "Item " + strconv.Itoa(i), "Type": "Movie", "UserData": map[string]any{}}
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ExcludeItemIds") != "" || r.URL.Query().Get("IsPlayed") != "" {
			t.Fatalf("negative filter leaked personal query params: %s", r.URL.RawQuery)
		}
		start := intQuery(r.URL.Query(), "StartIndex", 0)
		limit := intQuery(r.URL.Query(), "Limit", len(items))
		end := start + limit
		if end > len(items) {
			end = len(items)
		}
		out := make([]any, 0, end-start)
		for _, item := range items[start:end] {
			out = append(out, item)
		}
		writeTestJSON(w, map[string]any{"Items": out, "TotalRecordCount": len(items), "StartIndex": start})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	for i := 0; i < 5; i++ {
		_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "item-" + strconv.Itoa(i), ItemType: "Movie", Played: true})
	}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items?api_key=gateway-token&IsPlayed=false&Limit=3", nil))
	defer resp.Body.Close()
	var body map[string]any
	decodeJSON(t, resp.Body, &body)
	ids := itemIDsFromResponse(body["Items"].([]any))
	if strings.Join(ids, ",") != "item-5,item-6,item-7" {
		t.Fatalf("negative filter ids = %v", ids)
	}
	if got := int(body["TotalRecordCount"].(float64)); got != 5 {
		t.Fatalf("TotalRecordCount = %d, want approximate 5", got)
	}
}

func TestNegativePersonalFilterWithoutLimitDoesNotTruncateAtFirstPage(t *testing.T) {
	items := make([]map[string]any, personalScanBatchLimit+5)
	for i := range items {
		items[i] = map[string]any{"Id": "item-" + strconv.Itoa(i), "Name": "Item", "Type": "Movie", "UserData": map[string]any{}}
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := intQuery(r.URL.Query(), "StartIndex", 0)
		limit := intQuery(r.URL.Query(), "Limit", len(items))
		end := start + limit
		if end > len(items) {
			end = len(items)
		}
		out := make([]any, 0, end-start)
		for _, item := range items[start:end] {
			out = append(out, item)
		}
		writeTestJSON(w, map[string]any{"Items": out, "TotalRecordCount": len(items)})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items?api_key=gateway-token&IsPlayed=false", nil))
	defer resp.Body.Close()
	var body map[string]any
	decodeJSON(t, resp.Body, &body)
	if got := len(body["Items"].([]any)); got != personalScanBatchLimit+5 {
		t.Fatalf("unlimited negative items = %d, want %d", got, personalScanBatchLimit+5)
	}
}

func TestNegativePersonalFilterDoesNotUndercountTotalWithBackendOnlyFilters(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"Items": []any{map[string]any{"Id": "visible", "Name": "Visible", "Type": "Movie", "UserData": map[string]any{}}}, "TotalRecordCount": 1})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "played-outside-search", ItemType: "Movie", Played: true})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items?api_key=gateway-token&IsPlayed=false&SearchTerm=visible&Limit=1", nil))
	defer resp.Body.Close()
	var body map[string]any
	decodeJSON(t, resp.Body, &body)
	if got := int(body["TotalRecordCount"].(float64)); got != 1 {
		t.Fatalf("TotalRecordCount = %d, want upstream total 1", got)
	}
}

func TestLatestBackfillsWhenInitialItemsArePlayed(t *testing.T) {
	var limits []int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := intQuery(r.URL.Query(), "Limit", 0)
		limits = append(limits, limit)
		items := make([]any, 0, limit)
		for i := 0; i < limit && i < 13; i++ {
			items = append(items, map[string]any{"Id": "latest-" + strconv.Itoa(i), "Name": "Latest", "Type": "Movie", "UserData": map[string]any{}})
		}
		writeTestJSON(w, items)
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	for i := 0; i < 10; i++ {
		_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "latest-" + strconv.Itoa(i), Played: true})
	}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items/Latest?api_key=gateway-token&Limit=3", nil))
	defer resp.Body.Close()
	var body []any
	decodeJSON(t, resp.Body, &body)
	ids := itemIDsFromResponse(body)
	if strings.Join(ids, ",") != "latest-10,latest-11,latest-12" {
		t.Fatalf("latest ids = %v", ids)
	}
	if len(limits) < 3 || limits[0] != 3 || limits[1] != 9 || limits[2] != 27 {
		t.Fatalf("latest backfill limits = %v", limits)
	}
}

func TestLatestLargeLimitIsCappedInsteadOfReturningEmpty(t *testing.T) {
	var sawLimit int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawLimit = intQuery(r.URL.Query(), "Limit", 0)
		writeTestJSON(w, []any{map[string]any{"Id": "latest-1", "Name": "Latest", "Type": "Movie", "UserData": map[string]any{}}})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items/Latest?api_key=gateway-token&Limit=999", nil))
	defer resp.Body.Close()
	var body []any
	decodeJSON(t, resp.Body, &body)
	if len(body) != 1 || sawLimit != latestBackfillLimit {
		t.Fatalf("latest body len=%d backend limit=%d, want 1/%d", len(body), sawLimit, latestBackfillLimit)
	}
}

func TestPlaybackReportsCanUseFormBodyAndQueryParameters(t *testing.T) {
	var forwarded int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded++
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	form := strings.NewReader("PositionTicks=300&RunTimeTicks=1000")
	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Progress?api_key=gateway-token&ItemId=form-item", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("form playback status = %d", resp.StatusCode)
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "form-item")
	if err != nil || state.PlaybackPositionTicks != 300 || state.RunTimeTicks != 1000 {
		t.Fatalf("form playback state = %#v err=%v", state, err)
	}

	resp = do(t, mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Progress?api_key=gateway-token&ItemId=query-item&PlaybackPositionTicks=700", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("query playback status = %d", resp.StatusCode)
	}
	state, err = store.FindPlaybackState(context.Background(), "u1", "query-item")
	if err != nil || state.PlaybackPositionTicks != 700 {
		t.Fatalf("query playback state = %#v err=%v", state, err)
	}

	jsonReq := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Progress?api_key=gateway-token&ItemId=json-query-item", strings.NewReader(`{"PositionTicks":900,"RunTimeTicks":1800}`))
	jsonReq.Header.Set("Content-Type", "application/json")
	resp = do(t, jsonReq)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("json/query playback status = %d", resp.StatusCode)
	}
	state, err = store.FindPlaybackState(context.Background(), "u1", "json-query-item")
	if err != nil || state.PlaybackPositionTicks != 900 || state.RunTimeTicks != 1800 {
		t.Fatalf("json/query playback state = %#v err=%v", state, err)
	}
	if forwarded != 0 {
		t.Fatalf("forwarded playback reports = %d, want 0", forwarded)
	}
}

func TestJSONOverlayRunsWhenContentTypeIsMissing(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = []string{""}
		_, _ = w.Write([]byte(`{"Items":[{"Id":"movie-1","Name":"Movie","Type":"Movie","UserData":{"Played":true}}]}`))
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items?api_key=gateway-token", nil))
	defer resp.Body.Close()
	var body map[string]any
	decodeJSON(t, resp.Body, &body)
	items := body["Items"].([]any)
	userData := items[0].(map[string]any)["UserData"].(map[string]any)
	if userData["Played"] != false {
		t.Fatalf("missing content-type JSON was not overlaid: %#v", userData)
	}
}

func TestAggregateWithoutTrustedChildCountDoesNotReportComplete(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ParentId") != "" {
			writeTestJSON(w, map[string]any{"Items": []any{}, "TotalRecordCount": 0})
			return
		}
		writeTestJSON(w, map[string]any{"Items": []any{map[string]any{"Id": "show-1", "Name": "Show", "Type": "Series", "UserData": map[string]any{"Played": true, "UnplayedItemCount": float64(0)}}}})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "ep-1", SeriesID: "show-1", Played: true})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items?api_key=gateway-token", nil))
	defer resp.Body.Close()
	var body map[string]any
	decodeJSON(t, resp.Body, &body)
	userData := body["Items"].([]any)[0].(map[string]any)["UserData"].(map[string]any)
	if userData["Played"] == true || userData["UnplayedItemCount"] != nil {
		t.Fatalf("aggregate without trusted total should not report complete: %#v", userData)
	}
}

func itemIDsFromResponse(items []any) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		m, _ := item.(map[string]any)
		id, _ := stringField(m, "Id")
		ids = append(ids, id)
	}
	return ids
}

func TestNextUpUsesGatewaySeriesState(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Shows/show-1/Episodes" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		if r.URL.Query().Get("Limit") != "" || r.URL.Query().Get("StartIndex") != "" {
			t.Fatalf("next up episode lookup should not forward pagination: %s", r.URL.RawQuery)
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

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Shows/NextUp?api_key=gateway-token&Limit=1", nil))
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
	session.DeviceID = "client-device"
	session.BackendIdentity = backendIdentityForTest("device-1")
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
	if resp.Header.Get(gatewayVersionHeader) == "" {
		t.Fatal("gateway version header was not set")
	}
	resp = do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/System/Ping", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("old base path status = %d, want 404", resp.StatusCode)
	}
}

func TestCredentialQueryXEmbyTokenProxiesBackendTokenOnly(t *testing.T) {
	const backendToken = "backend-token"
	var upstreamQuery url.Values
	var upstreamHeader string
	var backendHits int

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/emby/Users/AuthenticateByName" {
			writeTestJSON(w, map[string]any{
				"AccessToken": backendToken,
				"ServerId":    "backend-server",
				"User":        map[string]any{"Id": "backend-user", "Name": "shared"},
			})
			return
		}
		backendHits++
		upstreamQuery = r.URL.Query()
		upstreamHeader = r.Header.Get("X-Emby-Token")
		writeTestJSON(w, map[string]any{"Id": "backend-server"})
	}))
	defer backend.Close()

	selected, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}
	store := testStore(backend.URL + "/emby")
	session := testSession(backend.URL + "/emby")
	session.GatewayTokenHash = HashToken(selected)
	session.BackendToken = backendToken
	store.Sessions[session.GatewayTokenHash] = session
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/System/Info?X-Emby-Token="+url.QueryEscape(selected), nil))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if backendHits != 1 {
		t.Fatalf("backend hits = %d, want 1", backendHits)
	}
	if upstreamHeader != backendToken {
		t.Fatalf("upstream header token mismatch")
	}
	if got := upstreamQuery.Get("X-Emby-Token"); got != backendToken {
		t.Fatalf("upstream X-Emby-Token query = %q, want backend token", got)
	}
	if strings.Contains(upstreamQuery.Encode(), selected) {
		t.Fatal("upstream query leaked selected gateway token")
	}
}

func TestCredentialQueryNormalizationAndGuards(t *testing.T) {
	selected, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}
	otherActive, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}
	otherRevoked, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}
	otherExpired, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}
	unknownShaped, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}
	secondShaped, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}

	type captured struct {
		hits   int
		query  url.Values
		header string
	}

	for _, tc := range []struct {
		name            string
		rawQuery        string
		headerToken     string
		wantStatus      int
		wantBackendHits int
		wantFinds       int
		wantExists      int
		checkUpstream   func(t *testing.T, c captured)
		prepare         func(store *countingSessionStore)
	}{
		{
			name:            "header plus different strict query rewritten",
			rawQuery:        "api_key=other-strict&access_token=another&X-Emby-Token=third",
			headerToken:     selected,
			wantStatus:      http.StatusOK,
			wantBackendHits: 1,
			wantFinds:       1,
			checkUpstream: func(t *testing.T, c captured) {
				t.Helper()
				for _, key := range []string{"api_key", "access_token", "X-Emby-Token"} {
					for _, value := range c.query[key] {
						if value != "backend-token" {
							t.Fatalf("%s values = %v, want all backend-token", key, c.query[key])
						}
					}
				}
			},
		},
		{
			name:            "header plus opaque generic preserved without extra lookup",
			rawQuery:        "token=cdn-signature&signature=keep",
			headerToken:     selected,
			wantStatus:      http.StatusOK,
			wantBackendHits: 1,
			wantFinds:       1,
			checkUpstream: func(t *testing.T, c captured) {
				t.Helper()
				if c.query.Get("token") != "cdn-signature" || c.query.Get("signature") != "keep" {
					t.Fatalf("opaque query not preserved: %v", c.query)
				}
			},
		},
		{
			name:            "selected under arbitrary signature key rewritten",
			rawQuery:        "signature=" + url.QueryEscape(selected) + "&token=cdn-signature",
			headerToken:     selected,
			wantStatus:      http.StatusOK,
			wantBackendHits: 1,
			wantFinds:       1,
			checkUpstream: func(t *testing.T, c captured) {
				t.Helper()
				if c.query.Get("signature") != "backend-token" {
					t.Fatalf("signature = %q, want backend-token", c.query.Get("signature"))
				}
				if c.query.Get("token") != "cdn-signature" {
					t.Fatalf("opaque token = %q", c.query.Get("token"))
				}
				if strings.Contains(c.query.Encode(), selected) {
					t.Fatal("upstream query leaked selected gateway token")
				}
			},
		},
		{
			name:            "selected generic token rewritten",
			rawQuery:        "token=" + url.QueryEscape(selected),
			wantStatus:      http.StatusOK,
			wantBackendHits: 1,
			wantFinds:       1,
			checkUpstream: func(t *testing.T, c captured) {
				t.Helper()
				if c.query.Get("token") != "backend-token" {
					t.Fatalf("token = %q, want backend-token", c.query.Get("token"))
				}
			},
		},
		{
			name:            "unknown gateway-shaped generic preserved",
			rawQuery:        "token=" + url.QueryEscape(unknownShaped),
			headerToken:     selected,
			wantStatus:      http.StatusOK,
			wantBackendHits: 1,
			wantFinds:       1,
			wantExists:      1,
			checkUpstream: func(t *testing.T, c captured) {
				t.Helper()
				if c.query.Get("token") != unknownShaped {
					t.Fatalf("unknown shaped token was rewritten")
				}
			},
		},
		{
			name:            "active gateway-shaped generic rejected",
			rawQuery:        "token=" + url.QueryEscape(otherActive),
			headerToken:     selected,
			wantStatus:      http.StatusBadRequest,
			wantBackendHits: 0,
			wantFinds:       1,
			wantExists:      1,
			prepare: func(store *countingSessionStore) {
				session := testSession("http://backend/emby")
				session.GatewayTokenHash = HashToken(otherActive)
				store.Sessions[session.GatewayTokenHash] = session
			},
		},
		{
			name:            "revoked gateway-shaped generic rejected",
			rawQuery:        "token=" + url.QueryEscape(otherRevoked),
			headerToken:     selected,
			wantStatus:      http.StatusBadRequest,
			wantBackendHits: 0,
			wantFinds:       1,
			wantExists:      1,
			prepare: func(store *countingSessionStore) {
				session := testSession("http://backend/emby")
				session.GatewayTokenHash = HashToken(otherRevoked)
				now := time.Now().UTC()
				session.RevokedAt = &now
				store.Sessions[session.GatewayTokenHash] = session
			},
		},
		{
			name:            "expired gateway-shaped generic rejected",
			rawQuery:        "token=" + url.QueryEscape(otherExpired),
			headerToken:     selected,
			wantStatus:      http.StatusBadRequest,
			wantBackendHits: 0,
			wantFinds:       1,
			wantExists:      1,
			prepare: func(store *countingSessionStore) {
				session := testSession("http://backend/emby")
				session.GatewayTokenHash = HashToken(otherExpired)
				session.ExpiresAt = time.Now().UTC().Add(-time.Hour)
				store.Sessions[session.GatewayTokenHash] = session
			},
		},
		{
			name:            "duplicate same gateway-shaped value one exists lookup",
			rawQuery:        "token=" + url.QueryEscape(unknownShaped) + "&token=" + url.QueryEscape(unknownShaped),
			headerToken:     selected,
			wantStatus:      http.StatusOK,
			wantBackendHits: 1,
			wantFinds:       1,
			wantExists:      1,
		},
		{
			name:            "multiple different gateway-shaped values rejected without exists lookup",
			rawQuery:        "token=" + url.QueryEscape(unknownShaped) + "&token=" + url.QueryEscape(secondShaped),
			headerToken:     selected,
			wantStatus:      http.StatusBadRequest,
			wantBackendHits: 0,
			wantFinds:       1,
			wantExists:      0,
		},
		{
			name:            "empty strict values preserved",
			rawQuery:        "api_key=&api_key=" + url.QueryEscape(selected) + "&access_token=",
			wantStatus:      http.StatusOK,
			wantBackendHits: 1,
			wantFinds:       1,
			checkUpstream: func(t *testing.T, c captured) {
				t.Helper()
				if got := c.query["api_key"]; len(got) != 2 || got[0] != "" || got[1] != "backend-token" {
					t.Fatalf("api_key = %v", got)
				}
				if got := c.query["access_token"]; len(got) != 1 || got[0] != "" {
					t.Fatalf("access_token = %v", got)
				}
			},
		},
		{
			name:            "malformed query rejected without auth before session lookup",
			rawQuery:        "api_key=%ZZ",
			wantStatus:      http.StatusBadRequest,
			wantBackendHits: 0,
			wantFinds:       0,
			wantExists:      0,
		},
		{
			name:            "malformed query rejected with header before session lookup",
			rawQuery:        "api_key=%ZZ",
			headerToken:     selected,
			wantStatus:      http.StatusBadRequest,
			wantBackendHits: 0,
			wantFinds:       0,
			wantExists:      0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c captured
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/emby/Users/AuthenticateByName" {
					writeTestJSON(w, map[string]any{
						"AccessToken": "backend-token",
						"ServerId":    "backend-server",
						"User":        map[string]any{"Id": "backend-user", "Name": "shared"},
					})
					return
				}
				c.hits++
				c.query = r.URL.Query()
				c.header = r.Header.Get("X-Emby-Token")
				writeTestJSON(w, map[string]any{"Id": "backend-server"})
			}))
			defer backend.Close()

			base := testStore(backend.URL + "/emby")
			store := &countingSessionStore{MemoryStore: base}
			session := testSession(backend.URL + "/emby")
			session.GatewayTokenHash = HashToken(selected)
			session.BackendToken = "backend-token"
			store.Sessions[session.GatewayTokenHash] = session
			if tc.prepare != nil {
				tc.prepare(store)
			}
			gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
			defer gw.Close()

			req := mustRequest(t, http.MethodGet, gw.URL+"/emby/System/Info?"+tc.rawQuery, nil)
			if tc.headerToken != "" {
				req.Header.Set("X-Emby-Token", tc.headerToken)
			}
			beforeFinds := store.findCountLocked()
			beforeExists := store.existsCountLocked()
			resp := do(t, req)
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d body=%s", resp.StatusCode, tc.wantStatus, body)
			}
			if c.hits != tc.wantBackendHits {
				t.Fatalf("backend hits = %d, want %d", c.hits, tc.wantBackendHits)
			}
			finds := store.findCountLocked() - beforeFinds
			exists := store.existsCountLocked() - beforeExists
			if finds != tc.wantFinds || exists != tc.wantExists {
				t.Fatalf("lookups finds=%d wantFinds=%d exists=%d wantExists=%d (findHashes=%v existsHashes=%v)",
					finds, tc.wantFinds, exists, tc.wantExists, store.hashesLocked(), store.existsHashesLocked())
			}
			if strings.Contains(string(body), selected) || strings.Contains(string(body), otherActive) {
				t.Fatal("response body leaked a token value")
			}
			if tc.checkUpstream != nil {
				tc.checkUpstream(t, c)
			}
		})
	}
}

func TestCredentialQueryStoreErrorReturns503WithoutUpstream(t *testing.T) {
	selected, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}
	conflict, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}

	var backendHits int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHits++
		t.Fatalf("backend should not be dialed: %s %s", r.Method, r.URL.String())
	}))
	defer backend.Close()

	base := testStore(backend.URL + "/emby")
	store := &storeErrorOnMissingSession{MemoryStore: base}
	session := testSession(backend.URL + "/emby")
	session.GatewayTokenHash = HashToken(selected)
	session.BackendToken = "backend-token"
	store.Sessions[session.GatewayTokenHash] = session

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	req := mustRequest(t, http.MethodGet, gw.URL+"/emby/System/Info?token="+url.QueryEscape(conflict), nil)
	req.Header.Set("X-Emby-Token", selected)
	resp := do(t, req)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 body=%s", resp.StatusCode, body)
	}
	if backendHits != 0 {
		t.Fatalf("backend hits = %d, want 0", backendHits)
	}
	if strings.Contains(string(body), selected) || strings.Contains(string(body), conflict) {
		t.Fatal("response body leaked a token value")
	}
}

func TestCredentialQueryLogoutRevokesSelectedToken(t *testing.T) {
	selected, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}
	store := testStore("http://backend/emby")
	session := testSession("http://backend/emby")
	session.GatewayTokenHash = HashToken(selected)
	store.Sessions[session.GatewayTokenHash] = session
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	logout := do(t, mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Logout?X-Emby-Token="+url.QueryEscape(selected), nil))
	_ = logout.Body.Close()
	if logout.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d", logout.StatusCode)
	}
	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/System/Info?api_key="+url.QueryEscape(selected), nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-logout status = %d, want 401", resp.StatusCode)
	}
}

func TestCredentialQueryLogoutMalformedQueryReturns400(t *testing.T) {
	store := &countingOpsStore{MemoryStore: testStore("http://backend/emby")}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Logout?api_key=%ZZ", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if store.pathPolicy != 0 || store.finds != 0 || store.exists != 0 || store.audits != 0 {
		t.Fatalf("store ops path=%d finds=%d exists=%d audits=%d, want all 0", store.pathPolicy, store.finds, store.exists, store.audits)
	}
}

func TestCredentialQueryMalformedProxySkipsAllStoreOps(t *testing.T) {
	var backendHits int
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		backendHits++
	}))
	defer backend.Close()

	store := &countingOpsStore{MemoryStore: testStore(backend.URL + "/emby")}
	store.PathPolicies = []PathPolicy{{
		ID: "deny-all", Method: "*", Path: "/**", Action: "deny", Priority: 1, Enabled: true,
	}}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/System/Info?api_key=%ZZ", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if backendHits != 0 {
		t.Fatalf("backend hits = %d", backendHits)
	}
	if store.pathPolicy != 0 || store.finds != 0 || store.exists != 0 || store.audits != 0 {
		t.Fatalf("store ops path=%d finds=%d exists=%d audits=%d, want all 0", store.pathPolicy, store.finds, store.exists, store.audits)
	}
}

func TestCredentialQueryLogoutSelectedOnlyIgnoresLowerPriorityToken(t *testing.T) {
	selected, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}
	other, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}

	store := &countingSessionStore{MemoryStore: testStore("http://backend/emby")}
	selectedSession := testSession("http://backend/emby")
	selectedSession.GatewayTokenHash = HashToken(selected)
	store.Sessions[selectedSession.GatewayTokenHash] = selectedSession
	otherSession := testSession("http://backend/emby")
	otherSession.GatewayTokenHash = HashToken(other)
	store.Sessions[otherSession.GatewayTokenHash] = otherSession

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Logout?token="+url.QueryEscape(other), nil)
	req.Header.Set("X-Emby-Token", selected)
	beforeExists := store.existsCountLocked()
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d", resp.StatusCode)
	}
	if store.existsCountLocked() != beforeExists {
		t.Fatalf("logout performed exists lookups = %d", store.existsCountLocked()-beforeExists)
	}

	selectedFound, err := store.FindSessionByTokenHash(context.Background(), HashToken(selected))
	if err != nil {
		t.Fatalf("selected session lookup: %v", err)
	}
	if selectedFound.Active(time.Now().UTC()) {
		t.Fatal("selected session should be revoked")
	}
	otherFound, err := store.FindSessionByTokenHash(context.Background(), HashToken(other))
	if err != nil {
		t.Fatalf("other session lookup: %v", err)
	}
	if !otherFound.Active(time.Now().UTC()) {
		t.Fatal("lower-priority token session should remain active")
	}
}

func TestCredentialQueryWebSocketGuardAndAPIKey(t *testing.T) {
	selected, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}
	conflict, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}

	t.Run("api_key upgrade", func(t *testing.T) {
		var sawBackend bool
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/emby/Users/AuthenticateByName":
				writeTestJSON(w, map[string]any{
					"AccessToken": "backend-token",
					"ServerId":    "backend-server",
					"User":        map[string]any{"Id": "backend-user", "Name": "shared"},
				})
			case "/emby/socket":
				if r.URL.Query().Get("api_key") == "backend-token" && r.Header.Get("X-Emby-Token") == "backend-token" {
					sawBackend = true
				}
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Fatal("backend cannot hijack")
				}
				conn, rw, err := hj.Hijack()
				if err != nil {
					t.Fatalf("hijack: %v", err)
				}
				defer conn.Close()
				_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
				_ = rw.Flush()
			default:
				t.Fatalf("unexpected backend path %s", r.URL.Path)
			}
		}))
		defer backend.Close()

		store := testStore(backend.URL + "/emby")
		session := testSession(backend.URL + "/emby")
		session.GatewayTokenHash = HashToken(selected)
		session.BackendToken = "backend-token"
		store.Sessions[session.GatewayTokenHash] = session
		gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
		defer gw.Close()

		gwURL, err := url.Parse(gw.URL)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.Dial("tcp", gwURL.Host)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("GET /emby/socket?api_key=" + selected + " HTTP/1.1\r\n" +
			"Host: " + gwURL.Host + "\r\n" +
			"Connection: Upgrade\r\n" +
			"Upgrade: websocket\r\n" +
			"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
			"Sec-WebSocket-Version: 13\r\n\r\n"))
		status, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(status, "101") {
			t.Fatalf("status line = %q", status)
		}
		if !sawBackend {
			t.Fatal("backend did not receive rewritten websocket credentials")
		}
	})

	t.Run("conflict does not dial", func(t *testing.T) {
		var backendHits int
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			backendHits++
			t.Fatalf("backend should not be dialed: %s %s", r.Method, r.URL.String())
		}))
		defer backend.Close()

		store := testStore(backend.URL + "/emby")
		session := testSession(backend.URL + "/emby")
		session.GatewayTokenHash = HashToken(selected)
		store.Sessions[session.GatewayTokenHash] = session
		conflictSession := testSession(backend.URL + "/emby")
		conflictSession.GatewayTokenHash = HashToken(conflict)
		store.Sessions[conflictSession.GatewayTokenHash] = conflictSession
		gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
		defer gw.Close()

		gwURL, err := url.Parse(gw.URL)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.Dial("tcp", gwURL.Host)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("GET /emby/socket?api_key=" + selected + "&token=" + conflict + " HTTP/1.1\r\n" +
			"Host: " + gwURL.Host + "\r\n" +
			"Connection: Upgrade\r\n" +
			"Upgrade: websocket\r\n" +
			"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
			"Sec-WebSocket-Version: 13\r\n\r\n"))
		status, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(status, "400") {
			t.Fatalf("status line = %q, want 400", status)
		}
		if backendHits != 0 {
			t.Fatalf("backend hits = %d", backendHits)
		}
	})
}

type countingSessionStore struct {
	*MemoryStore
	mu           sync.Mutex
	hashes       []string
	existsHashes []string
}

func (c *countingSessionStore) FindSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error) {
	c.mu.Lock()
	c.hashes = append(c.hashes, tokenHash)
	c.mu.Unlock()
	return c.MemoryStore.FindSessionByTokenHash(ctx, tokenHash)
}

func (c *countingSessionStore) SessionTokenExists(ctx context.Context, tokenHash string) (bool, error) {
	c.mu.Lock()
	c.existsHashes = append(c.existsHashes, tokenHash)
	c.mu.Unlock()
	return c.MemoryStore.SessionTokenExists(ctx, tokenHash)
}

func (c *countingSessionStore) findCountLocked() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.hashes)
}

func (c *countingSessionStore) existsCountLocked() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.existsHashes)
}

func (c *countingSessionStore) hashesLocked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.hashes...)
}

func (c *countingSessionStore) existsHashesLocked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.existsHashes...)
}

type countingOpsStore struct {
	*MemoryStore
	pathPolicy int
	finds      int
	exists     int
	audits     int
}

func (c *countingOpsStore) CheckPathPolicy(ctx context.Context, method, relativePath string) (PathPolicyDecision, error) {
	c.pathPolicy++
	return c.MemoryStore.CheckPathPolicy(ctx, method, relativePath)
}

func (c *countingOpsStore) FindSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error) {
	c.finds++
	return c.MemoryStore.FindSessionByTokenHash(ctx, tokenHash)
}

func (c *countingOpsStore) SessionTokenExists(ctx context.Context, tokenHash string) (bool, error) {
	c.exists++
	return c.MemoryStore.SessionTokenExists(ctx, tokenHash)
}

func (c *countingOpsStore) RecordAudit(ctx context.Context, entry AuditLog) error {
	c.audits++
	return c.MemoryStore.RecordAudit(ctx, entry)
}

// storeErrorOnMissingSession keeps known sessions working for auth, but turns
// missing conflict existence checks into a store failure so the guard can return 503.
type storeErrorOnMissingSession struct {
	*MemoryStore
}

func (s *storeErrorOnMissingSession) SessionTokenExists(ctx context.Context, tokenHash string) (bool, error) {
	exists, err := s.MemoryStore.SessionTokenExists(ctx, tokenHash)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, errors.New("session index unavailable")
	}
	return true, nil
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

type countingPlaybackStore struct {
	*MemoryStore
	singleLookups int
	batchLookups  int
	batchItemIDs  []string
}

func (c *countingPlaybackStore) FindPlaybackState(ctx context.Context, gatewayUserID, itemID string) (*PlaybackState, error) {
	c.singleLookups++
	return c.MemoryStore.FindPlaybackState(ctx, gatewayUserID, itemID)
}

func (c *countingPlaybackStore) ListPlaybackStatesByItemIDs(ctx context.Context, gatewayUserID string, itemIDs []string) (map[string]*PlaybackState, error) {
	c.batchLookups++
	c.batchItemIDs = append([]string(nil), itemIDs...)
	return c.MemoryStore.ListPlaybackStatesByItemIDs(ctx, gatewayUserID, itemIDs)
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

func backendIdentityForTest(deviceID string) BackendClientIdentity {
	identity := DefaultBackendClientIdentity()
	identity.DeviceID = deviceID
	return identity
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
