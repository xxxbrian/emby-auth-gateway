package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSnapshotFailedHTTPRefreshFailsClosedWithoutLeakingUpstreamIdentity(t *testing.T) {
	server := NewServer(Config{GatewayServerID: "gateway-server"}, NewMemoryStore())
	session := testSession()
	upstream := upstreamRequestSnapshot{baseURL: "https://old.example/emby", serverID: "backend-server", userID: "backend-user", token: "backend-token", identity: backendIdentityForTest("backend-device")}
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/a", nil)
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header: http.Header{
			"Content-Length":   {"999"},
			"Content-Range":    {"bytes 0-998/999"},
			"Content-MD5":      {"backend-token"},
			"Digest":           {"backend-server"},
			"ETag":             {`"backend-token"`},
			"Last-Modified":    {"yesterday"},
			"Content-Location": {"https://old.example/emby/Items/a"},
			"Location":         {"https://old.example/emby/Videos/backend-user/stream?api_key=backend-token"},
		},
		Body:    io.NopCloser(strings.NewReader(`{"AccessToken":"backend-token","ServerId":"backend-server","UserId":"backend-user","Url":"https://old.example/emby"}`)),
		Request: req,
	}
	w := httptest.NewRecorder()
	server.writeProxyResponseWithSnapshot(w, req, "/Items/a", resp, session, upstream, "gateway-token", "https://gateway.test/emby")

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%q", w.Code, http.StatusBadGateway, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var output strings.Builder
	output.WriteString(w.Body.String())
	for _, values := range w.Header() {
		output.WriteString(strings.Join(values, "\n"))
	}
	for _, leak := range []string{"backend-token", upstream.baseURL, upstream.serverID, upstream.userID, "gateway-token"} {
		if strings.Contains(output.String(), leak) {
			t.Fatalf("failed-refresh response leaked %q: body=%q headers=%v", leak, w.Body.String(), w.Header())
		}
	}
	for _, name := range []string{"Content-Length", "Content-Range", "Content-MD5", "Digest", "ETag", "Last-Modified", "Content-Location", "Location"} {
		if got := w.Header().Get(name); got != "" {
			t.Fatalf("unsafe or stale header %s survived with value %q", name, got)
		}
	}
	lowerBody := strings.ToLower(w.Body.String())
	for _, detail := range []string{"parse", "projection"} {
		if strings.Contains(lowerBody, detail) {
			t.Fatalf("response exposed %s details: %q", detail, w.Body.String())
		}
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
