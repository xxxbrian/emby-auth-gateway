package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSnapshotFailedHTTPRefreshKeepsOriginalResponseSanitization(t *testing.T) {
	server := NewServer(Config{GatewayServerID: "gateway-server"}, NewMemoryStore())
	session := testSession()
	upstream := upstreamRequestSnapshot{baseURL: "https://old.example/emby", serverID: "backend-server", userID: "backend-user", token: "backend-token", identity: backendIdentityForTest("backend-device")}
	// A failed refresh preserves this request attempt's immutable snapshot.
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/a", nil)
	resp := &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Location": {"https://old.example/emby/Videos/backend-user/stream?api_key=backend-token"}}, Body: io.NopCloser(strings.NewReader(`{"AccessToken":"backend-token","ServerId":"backend-server","UserId":"backend-user"}`)), Request: req}
	w := httptest.NewRecorder()
	server.writeProxyResponseWithSnapshot(w, req, "/Items/a", resp, session, upstream, "gateway-token", "https://gateway.test/emby")
	output := w.Body.String() + w.Header().Get("Location")
	if strings.Contains(output, "backend-token") || strings.Contains(output, "old.example") || !strings.Contains(output, "gateway-token") {
		t.Fatalf("failed-refresh response leaked or missed snapshot values: %q", output)
	}
}

func TestSnapshotFailedWebSocketPreflightRetainsOriginalSnapshot(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/System/Info":
			w.WriteHeader(http.StatusUnauthorized)
		case "/emby/Users/AuthenticateByName":
			http.Error(w, "refresh failed", http.StatusBadGateway)
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
	}))
	defer backend.Close()
	store := testStore(backend.URL + "/emby")
	session := testSession()
	upstream := upstreamRequestSnapshot{baseURL: backend.URL + "/emby", serverID: "backend-server", userID: "backend-user", token: "backend-token", identity: backendIdentityForTest("backend-device")}
	server := NewServer(Config{HTTPClient: backend.Client()}, store)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/embywebsocket", nil)
	got := server.prepareBackendUpgrade(context.Background(), req, "/embywebsocket", session, upstream)
	if got != upstream {
		t.Fatalf("preflight snapshot changed on failed refresh: %#v", got)
	}
}

func TestSnapshotSuccessfulRefreshUsesCompleteReplacement(t *testing.T) {
	server := NewServer(Config{}, NewMemoryStore())
	session := testSession()
	old := upstreamRequestSnapshot{baseURL: "https://old.example/emby", serverID: "old-server", userID: "old-user", token: "old-token", identity: backendIdentityForTest("old-device")}
	newSnapshot := upstreamRequestSnapshot{baseURL: "https://new.example/emby", serverID: "new-server", userID: "new-user", token: "new-token", identity: BackendClientIdentity{UserAgent: "new-agent", Client: "new-client", Device: "new-device", DeviceID: "new-device-id", Version: "new-version"}}
	q, err := server.proxyURL(newSnapshot, session, "/Users/"+session.SyntheticUserID+"/Items", "api_key=gateway-token", "gateway-token")
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	server.rewriteRequestHeaders(h, newSnapshot)
	body := server.rewriteRequestBodyData([]byte(`{"Token":"gateway-token","UserId":"`+session.SyntheticUserID+`"}`), session, newSnapshot, "gateway-token")
	if q.Host != "new.example" || q.Query().Get("api_key") != "new-token" || !strings.Contains(h.Get("X-Emby-Authorization"), `UserId="new-user"`) || h.Get("User-Agent") != "new-agent" || !strings.Contains(readString(body), "new-token") || strings.Contains(q.String()+h.Get("X-Emby-Authorization"), old.token) {
		t.Fatalf("retry mixed snapshots: url=%q headers=%#v body=%q", q.String(), h, readString(server.rewriteRequestBodyData([]byte(`x`), session, newSnapshot, "gateway-token")))
	}
}

func TestSnapshotUserdataRewriteUsesPayloadSnapshotAfterRefresh(t *testing.T) {
	store := NewMemoryStore()
	server := NewServer(Config{GatewayServerID: "gateway-server"}, store)
	session := testSession()
	payloadSnapshot := upstreamRequestSnapshot{baseURL: "https://new.example/emby", serverID: "new-server", userID: "new-user", token: "new-token", identity: backendIdentityForTest("new-device")}
	// A later runtime must not affect the payload that came from the retry.
	laterRuntime := upstreamRequestSnapshot{baseURL: "https://later.example/emby", serverID: "later-server", userID: "later-user", token: "later-token", identity: backendIdentityForTest("later-device")}
	v := map[string]any{"AccessToken": "new-token", "ServerId": "new-server", "UserId": "new-user", "DirectStreamUrl": "https://new.example/emby/Videos/a/stream?api_key=new-token"}
	got := server.rewriteProxyJSONValueForRequestWithSnapshot(context.Background(), nil, v, session, payloadSnapshot, "gateway-token", "https://gateway.test/emby").(map[string]any)
	if got["AccessToken"] != "gateway-token" || got["ServerId"] != "gateway-server" || got["UserId"] != session.SyntheticUserID || strings.Contains(got["DirectStreamUrl"].(string), "new-token") || strings.Contains(got["DirectStreamUrl"].(string), laterRuntime.baseURL) {
		t.Fatalf("userdata rewrite used wrong snapshot: %#v", got)
	}
}

func readString(r interface{ Read([]byte) (int, error) }) string {
	b := make([]byte, 256)
	n, _ := r.Read(b)
	return string(b[:n])
}
