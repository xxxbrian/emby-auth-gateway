package main

import (
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
	_ "github.com/xxxbrian/emby-auth-gateway/internal/pbmigrations"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func TestCleanupPlaybackEventsKeepsOnlyRecentEvents(t *testing.T) {
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "test",
	})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	defer app.Cleanup()

	userID := createTestUser(t, app)
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	createPlaybackEvent(t, app, userID, "old", now.Add(-7*time.Hour))
	createPlaybackEvent(t, app, userID, "recent", now.Add(-5*time.Hour))

	if err := cleanupPlaybackEvents(app, now); err != nil {
		t.Fatalf("cleanup playback events: %v", err)
	}

	records, err := app.FindAllRecords("playback_events")
	if err != nil {
		t.Fatalf("query playback events: %v", err)
	}
	if len(records) != 1 || records[0].GetString("item_id") != "recent" {
		t.Fatalf("remaining playback events = %#v, want only recent", records)
	}
}

func TestCleanupGatewaySessionsKeepsOnlyRecentActiveOrRevokedSessions(t *testing.T) {
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "test",
	})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	defer app.Cleanup()

	userID := createTestUser(t, app)
	accountID := createTestBackendAccount(t, app)
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	createGatewaySession(t, app, userID, accountID, "expired-old", now.Add(-8*24*time.Hour), nil)
	revokedOld := now.Add(-8 * 24 * time.Hour)
	createGatewaySession(t, app, userID, accountID, "revoked-old", now.Add(24*time.Hour), &revokedOld)
	revokedRecent := now.Add(-6 * 24 * time.Hour)
	createGatewaySession(t, app, userID, accountID, "revoked-recent", now.Add(24*time.Hour), &revokedRecent)
	createGatewaySession(t, app, userID, accountID, "active", now.Add(24*time.Hour), nil)

	if err := cleanupGatewaySessions(app, now); err != nil {
		t.Fatalf("cleanup gateway sessions: %v", err)
	}

	records, err := app.FindAllRecords("gateway_sessions")
	if err != nil {
		t.Fatalf("query gateway sessions: %v", err)
	}
	remaining := map[string]bool{}
	for _, record := range records {
		remaining[record.GetString("gateway_token_hash")] = true
	}
	if len(remaining) != 2 || !remaining["revoked-recent"] || !remaining["active"] {
		t.Fatalf("remaining gateway sessions = %#v, want revoked-recent and active", remaining)
	}
}

func TestBackendIdentityDefaultsArePersistedOnServerCreate(t *testing.T) {
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "test",
	})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	defer app.Cleanup()
	registerBackendIdentityDefaults(app)

	servers, err := app.FindCollectionByNameOrId("emby_servers")
	if err != nil {
		t.Fatalf("find emby_servers: %v", err)
	}
	record := core.NewRecord(servers)
	record.Set("name", "server")
	record.Set("base_url", "https://emby.example.com")
	record.Set("enabled", true)
	if err := app.Save(record); err != nil {
		t.Fatalf("save server: %v", err)
	}

	defaults := gateway.DefaultBackendClientIdentity()
	if record.GetString("backend_user_agent") != defaults.UserAgent || record.GetString("backend_authorization_client") != defaults.Client || record.GetString("backend_authorization_device") != defaults.Device || record.GetString("backend_authorization_version") != defaults.Version {
		t.Fatalf("backend identity defaults not persisted: %#v", record.FieldsData())
	}
	if record.GetString("backend_authorization_device_id") != gateway.StableBackendDeviceID(record.Id) {
		t.Fatalf("backend device id = %q, want stable id from record id", record.GetString("backend_authorization_device_id"))
	}
}

func createTestUser(t *testing.T, app core.App) string {
	t.Helper()
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users: %v", err)
	}
	record := core.NewRecord(users)
	record.Set("username", "alice")
	record.Set("email", "alice@example.com")
	record.Set("synthetic_user_id", "gateway-user")
	record.Set("enabled", true)
	record.SetPassword("test-pass")
	if err := app.Save(record); err != nil {
		t.Fatalf("save user: %v", err)
	}
	return record.Id
}

func createPlaybackEvent(t *testing.T, app core.App, userID, itemID string, occurredAt time.Time) {
	t.Helper()
	events, err := app.FindCollectionByNameOrId("playback_events")
	if err != nil {
		t.Fatalf("find playback_events: %v", err)
	}
	record := core.NewRecord(events)
	record.Set("gateway_user", userID)
	record.Set("synthetic_user_id", "gateway-user")
	record.Set("item_id", itemID)
	record.Set("event", "progress")
	record.Set("occurred_at", occurredAt)
	if err := app.Save(record); err != nil {
		t.Fatalf("save playback event: %v", err)
	}
}

func createTestBackendAccount(t *testing.T, app core.App) string {
	t.Helper()
	servers, err := app.FindCollectionByNameOrId("emby_servers")
	if err != nil {
		t.Fatalf("find emby_servers: %v", err)
	}
	server := core.NewRecord(servers)
	server.Set("name", "test")
	server.Set("base_url", "http://127.0.0.1:8096/emby")
	identity := gateway.DefaultBackendClientIdentity()
	server.Set("backend_user_agent", identity.UserAgent)
	server.Set("backend_authorization_client", identity.Client)
	server.Set("backend_authorization_device", identity.Device)
	server.Set("backend_authorization_device_id", gateway.StableBackendDeviceID("test-server"))
	server.Set("backend_authorization_version", identity.Version)
	server.Set("enabled", true)
	if err := app.Save(server); err != nil {
		t.Fatalf("save server: %v", err)
	}

	accounts, err := app.FindCollectionByNameOrId("backend_accounts")
	if err != nil {
		t.Fatalf("find backend_accounts: %v", err)
	}
	account := core.NewRecord(accounts)
	account.Set("server", server.Id)
	account.Set("name", "backend")
	account.Set("backend_username", "shared")
	account.Set("backend_password", "password")
	account.Set("enabled", true)
	if err := app.Save(account); err != nil {
		t.Fatalf("save account: %v", err)
	}
	return account.Id
}

func createGatewaySession(t *testing.T, app core.App, userID, accountID, tokenHash string, expiresAt time.Time, revokedAt *time.Time) {
	t.Helper()
	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatalf("find gateway_sessions: %v", err)
	}
	record := core.NewRecord(sessions)
	record.Set("gateway_token_hash", tokenHash)
	record.Set("gateway_user", userID)
	record.Set("gateway_username", "alice")
	record.Set("synthetic_user_id", "gateway-user")
	record.Set("backend_account", accountID)
	record.Set("expires_at", expiresAt)
	if revokedAt != nil {
		record.Set("revoked_at", *revokedAt)
	}
	if err := app.Save(record); err != nil {
		t.Fatalf("save gateway session: %v", err)
	}
}
