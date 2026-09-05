package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
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
