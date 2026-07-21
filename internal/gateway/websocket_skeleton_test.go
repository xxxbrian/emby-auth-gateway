package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type noUpstreamLoadStore struct{ *MemoryStore }

func (s *noUpstreamLoadStore) LoadDefaultUpstreamRuntime(context.Context) (*UpstreamRuntime, error) {
	panic("upstream runtime must not be loaded")
}

func newWebSocketSkeletonServer(t *testing.T) (*Server, *countingRoundTripper) {
	t.Helper()
	store := testStore("http://127.0.0.1:1/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	transport := &countingRoundTripper{}
	return NewServer(Config{
		GatewayBasePath: "/emby",
		HTTPClient:      &http.Client{Transport: transport},
	}, &noUpstreamLoadStore{MemoryStore: store}), transport
}

func websocketSkeletonRequest(method, path string, upgrade bool) *http.Request {
	req := httptest.NewRequest(method, "http://gateway.test/emby"+path+"?api_key=gateway-token", nil)
	if upgrade {
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
	}
	return req
}

func TestLocalWebSocketSkeletonStatusesAndZeroUpstream(t *testing.T) {
	tests := []struct {
		name       string
		request    *http.Request
		status     int
		allow      string
		upgrade    string
		wantNoLoad bool
	}{
		{
			name:       "non-upgrade requires upgrade",
			request:    websocketSkeletonRequest(http.MethodGet, "/embywebsocket", false),
			status:     http.StatusUpgradeRequired,
			upgrade:    "websocket",
			wantNoLoad: true,
		},
		{
			name:       "wrong websocket method",
			request:    websocketSkeletonRequest(http.MethodPost, "/embywebsocket", true),
			status:     http.StatusMethodNotAllowed,
			allow:      http.MethodGet,
			wantNoLoad: true,
		},
		{
			name:       "nonlocal upgrade",
			request:    websocketSkeletonRequest(http.MethodGet, "/Items/item", true),
			status:     http.StatusNotFound,
			wantNoLoad: true,
		},
		{
			name:       "unhandled local personal",
			request:    websocketSkeletonRequest(http.MethodPut, "/Users/gateway-user/PlayedItems/item", false),
			status:     http.StatusNotFound,
			wantNoLoad: true,
		},
		{
			name:       "malformed local session command",
			request:    websocketSkeletonRequest(http.MethodPost, "/Sessions/public-id/Playing/Unknown", false),
			status:     http.StatusBadRequest,
			wantNoLoad: true,
		},
		{
			name:       "metadata wrong method",
			request:    websocketSkeletonRequest(http.MethodPost, "/System/Info", false),
			status:     http.StatusMethodNotAllowed,
			allow:      "GET, HEAD",
			wantNoLoad: true,
		},
		{
			name:       "media wrong method",
			request:    websocketSkeletonRequest(http.MethodPost, "/Items/item/Images/Primary", false),
			status:     http.StatusMethodNotAllowed,
			allow:      "GET, HEAD",
			wantNoLoad: true,
		},
		{
			name:       "negotiation wrong method",
			request:    websocketSkeletonRequest(http.MethodDelete, "/Items/item/PlaybackInfo", false),
			status:     http.StatusMethodNotAllowed,
			allow:      "GET, POST",
			wantNoLoad: true,
		},
		{
			name:       "unclassified unknown",
			request:    websocketSkeletonRequest(http.MethodGet, "/Unknown", false),
			status:     http.StatusNotFound,
			wantNoLoad: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, transport := newWebSocketSkeletonServer(t)
			writer := httptest.NewRecorder()
			server.ServeHTTP(writer, tt.request)
			if writer.Code != tt.status {
				t.Fatalf("status = %d, want %d", writer.Code, tt.status)
			}
			if got := writer.Header().Get("Allow"); got != tt.allow {
				t.Fatalf("Allow = %q, want %q", got, tt.allow)
			}
			if got := writer.Header().Get("Upgrade"); got != tt.upgrade {
				t.Fatalf("Upgrade = %q, want %q", got, tt.upgrade)
			}
			if tt.wantNoLoad && transport.hits != 0 {
				t.Fatalf("upstream transport hits = %d, want 0", transport.hits)
			}
		})
	}
}

func TestWebSocketInvalidCredentialsDoNotLoadOrDial(t *testing.T) {
	server, transport := newWebSocketSkeletonServer(t)
	for _, rawQuery := range []string{"api_key=bad", "api_key=%ZZ"} {
		req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/embywebsocket?"+rawQuery, nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		writer := httptest.NewRecorder()
		server.ServeHTTP(writer, req)
		if writer.Code != http.StatusUnauthorized && writer.Code != http.StatusBadRequest {
			t.Fatalf("query %q status = %d", rawQuery, writer.Code)
		}
	}
	if transport.hits != 0 {
		t.Fatalf("upstream transport hits = %d, want 0", transport.hits)
	}
}
