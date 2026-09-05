package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
)

// seedEndpointTestApp creates a test app with the canonical schema and seeds a
// default source plus one enabled default endpoint (as fresh setup would).
func seedEndpointTestApp(t *testing.T) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{DataDir: t.TempDir(), EncryptionEnv: "test"})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	t.Cleanup(app.Cleanup)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	// Seed source.
	srcCollection, err := app.FindCollectionByNameOrId("upstream_sources")
	if err != nil {
		t.Fatal(err)
	}
	source := core.NewRecord(srcCollection)
	for field, value := range map[string]any{
		"key": "default", "server_id": "server", "backend_username": "backend", "backend_password": "secret",
		"backend_user_agent": "agent", "backend_authorization_client": "client", "backend_authorization_device": "device",
		"backend_authorization_device_id": "device-id", "backend_authorization_version": "1.0",
	} {
		source.Set(field, value)
	}
	if err := app.Save(source); err != nil {
		t.Fatal(err)
	}
	// Seed primary endpoint (enabled default).
	epCollection, err := app.FindCollectionByNameOrId("upstream_endpoints")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := core.NewRecord(epCollection)
	endpoint.Set("source", source.Id)
	endpoint.Set("key", "primary")
	endpoint.Set("base_url", "https://primary.example")
	endpoint.Set("enabled", true)
	endpoint.Set("is_default", true)
	if err := app.Save(endpoint); err != nil {
		t.Fatal(err)
	}
	return app
}

func endpointByKey(t *testing.T, app core.App, key string) EndpointDTO {
	t.Helper()
	state, err := LoadUpstreamState(app)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range state.Endpoints {
		if record.GetString("key") == key {
			return endpointDTOFromRecord(record)
		}
	}
	t.Fatalf("endpoint %q not found", key)
	return EndpointDTO{}
}

func TestUpsertEndpointAddsSecondEndpointAndPromotesDefault(t *testing.T) {
	app := seedEndpointTestApp(t)
	dto, err := UpsertEndpoint(context.Background(), app, EndpointUpsertInput{Key: "cf", BaseURL: "https://cf.example", Enabled: true, IsDefault: false})
	if err != nil {
		t.Fatalf("upsert cf: %v", err)
	}
	if !dto.Enabled || dto.IsDefault || dto.Key != "cf" {
		t.Fatalf("cf dto = %#v", dto)
	}
	// Promote cf to default: primary must be cleared.
	if _, err := UpsertEndpoint(context.Background(), app, EndpointUpsertInput{ID: dto.ID, Key: "cf", BaseURL: "https://cf.example", Enabled: true, IsDefault: true}); err != nil {
		t.Fatalf("promote cf: %v", err)
	}
	if got := endpointByKey(t, app, "cf"); !got.IsDefault {
		t.Fatalf("cf not default after promote: %#v", got)
	}
	if got := endpointByKey(t, app, "primary"); got.IsDefault {
		t.Fatal("primary still default after promote")
	}
	if err := validateEndpointTopologyAfterMutation(app); err != nil {
		t.Fatalf("topology invalid: %v", err)
	}
}

func TestUpsertEndpointRejectsDuplicateKeyOrURL(t *testing.T) {
	app := seedEndpointTestApp(t)
	if _, err := UpsertEndpoint(context.Background(), app, EndpointUpsertInput{Key: "cf", BaseURL: "https://cf.example", Enabled: true, IsDefault: false}); err != nil {
		t.Fatalf("add cf: %v", err)
	}
	if _, err := UpsertEndpoint(context.Background(), app, EndpointUpsertInput{Key: "cf", BaseURL: "https://other.example", Enabled: true, IsDefault: false}); !errors.Is(err, ErrEndpointInvalid) {
		t.Fatalf("duplicate key error = %v", err)
	}
	if _, err := UpsertEndpoint(context.Background(), app, EndpointUpsertInput{Key: "cf2", BaseURL: "https://cf.example", Enabled: true, IsDefault: false}); !errors.Is(err, ErrEndpointInvalid) {
		t.Fatalf("duplicate url error = %v", err)
	}
}

func TestUpsertEndpointDisablingDefaultPromotesAnother(t *testing.T) {
	app := seedEndpointTestApp(t)
	if _, err := UpsertEndpoint(context.Background(), app, EndpointUpsertInput{Key: "cf", BaseURL: "https://cf.example", Enabled: true, IsDefault: false}); err != nil {
		t.Fatal(err)
	}
	primary := endpointByKey(t, app, "primary")
	if _, err := UpsertEndpoint(context.Background(), app, EndpointUpsertInput{ID: primary.ID, Key: "primary", BaseURL: primary.BaseURL, Enabled: false, IsDefault: false}); err != nil {
		t.Fatalf("disable primary: %v", err)
	}
	if got := endpointByKey(t, app, "cf"); !got.IsDefault || !got.Enabled {
		t.Fatalf("cf was not promoted: %#v", got)
	}
	if err := validateEndpointTopologyAfterMutation(app); err != nil {
		t.Fatalf("topology invalid: %v", err)
	}
}

func TestDeleteEndpointProtectsDefaultAndOnlyEndpoint(t *testing.T) {
	app := seedEndpointTestApp(t)
	cf, err := UpsertEndpoint(context.Background(), app, EndpointUpsertInput{Key: "cf", BaseURL: "https://cf.example", Enabled: true, IsDefault: false})
	if err != nil {
		t.Fatal(err)
	}
	primary := endpointByKey(t, app, "primary")
	if err := DeleteEndpoint(context.Background(), app, primary.ID); !errors.Is(err, ErrEndpointInvalid) {
		t.Fatalf("delete default with others error = %v", err)
	}
	if err := DeleteEndpoint(context.Background(), app, cf.ID); err != nil {
		t.Fatalf("delete non-default: %v", err)
	}
	if err := DeleteEndpoint(context.Background(), app, primary.ID); !errors.Is(err, ErrEndpointInvalid) {
		t.Fatalf("delete only endpoint error = %v", err)
	}
}

func TestProbeEndpointWebSocket(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			w.Header().Set("Connection", "Upgrade")
			w.Header().Set("Upgrade", "websocket")
			w.WriteHeader(http.StatusSwitchingProtocols)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if err := ProbeEndpointWebSocket(context.Background(), server.URL, "agent"); err != nil {
		t.Fatalf("websocket probe failed: %v", err)
	}

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer plain.Close()
	if err := ProbeEndpointWebSocket(context.Background(), plain.URL, "agent"); err == nil || !strings.Contains(err.Error(), "101") {
		t.Fatalf("expected non-websocket probe to fail with 101 hint, got %v", err)
	}
}

func TestRecordWebSocketProbeResult(t *testing.T) {
	app := seedEndpointTestApp(t)
	primary := endpointByKey(t, app, "primary")
	if err := RecordWebSocketProbeResult(context.Background(), app, primary.ID, true, nil); err != nil {
		t.Fatalf("record probe: %v", err)
	}
	updated := endpointByKey(t, app, "primary")
	if !updated.WebSocketCapable || updated.WebSocketProbedAt == nil || updated.WebSocketProbeError != "" {
		t.Fatalf("probe not persisted: %#v", updated)
	}
	if err := RecordWebSocketProbeResult(context.Background(), app, primary.ID, false, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	updated = endpointByKey(t, app, "primary")
	if updated.WebSocketCapable || updated.WebSocketProbeError != "boom" {
		t.Fatalf("failed probe not persisted: %#v", updated)
	}
}
