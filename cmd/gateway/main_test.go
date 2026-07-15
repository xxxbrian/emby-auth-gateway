package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
	_ "github.com/xxxbrian/emby-auth-gateway/internal/pbmigrations"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func TestVersionCommandPrintsBuildMetadata(t *testing.T) {
	cmd := newVersionCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute version command: %v", err)
	}
	text := out.String()
	for _, want := range []string{"version:", "commit:", "date:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("version output missing %q: %s", want, text)
		}
	}
}

func TestAnonymousImageConfigFromEnvRequiresPair(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		recordID, backendID       string
		hasRecordID, hasBackendID bool
		wantErr                   bool
	}{
		{"compose absent disabled", "", "", false, false, false},
		{"explicit blank pair", "", "", true, true, true},
		{"record only", "record", "", true, false, true},
		{"record plus blank backend", "record", "", true, true, true},
		{"backend only", "", "server", false, true, true},
		{"valid", " record ", " server ", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, err := anonymousImageConfigFromValues(tc.recordID, tc.hasRecordID, tc.backendID, tc.hasBackendID)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && tc.hasRecordID && (!config.configured || config.serverRecordID != "record" || config.backendServerID != "server") {
				t.Fatalf("config = %#v", config)
			}
		})
	}
}

type anonymousImageStartupValidatorStub struct{ err error }

func (s anonymousImageStartupValidatorStub) ValidateAnonymousImageNamespace(context.Context) error {
	return s.err
}

func TestValidateAnonymousImageStartup(t *testing.T) {
	staticErr := &gateway.AnonymousImageNamespaceError{Kind: gateway.AnonymousImageNamespaceStaticError, Err: errors.New("static")}
	mismatchErr := &gateway.AnonymousImageNamespaceError{Kind: gateway.AnonymousImageNamespaceMismatchError, Err: errors.New("mismatch")}
	transientErr := &gateway.AnonymousImageNamespaceError{Kind: gateway.AnonymousImageNamespaceTransientError, Err: errors.New("transient")}
	for _, tc := range []struct {
		name          string
		err           error
		wantTransient bool
		wantError     bool
	}{
		{"available", nil, false, false},
		{"transient mounts authenticated gateway", transientErr, true, false},
		{"static prevents mount", staticErr, false, true},
		{"mismatch prevents mount", mismatchErr, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transient, err := validateAnonymousImageStartup(anonymousImageStartupValidatorStub{err: tc.err})
			if transient != tc.wantTransient || (err != nil) != tc.wantError {
				t.Fatalf("transient/error = %v/%v", transient, err)
			}
		})
	}
}

func TestStartAnonymousImageNamespaceMountsOnlyWhenAllowed(t *testing.T) {
	staticErr := &gateway.AnonymousImageNamespaceError{Kind: gateway.AnonymousImageNamespaceStaticError, Err: errors.New("static")}
	mismatchErr := &gateway.AnonymousImageNamespaceError{Kind: gateway.AnonymousImageNamespaceMismatchError, Err: errors.New("mismatch")}
	transientErr := &gateway.AnonymousImageNamespaceError{Kind: gateway.AnonymousImageNamespaceTransientError, Err: errors.New("transient")}
	for _, tc := range []struct {
		name          string
		err           error
		wantMount     int
		wantTransient bool
	}{
		{"success", nil, 1, false},
		{"transient", transientErr, 1, true},
		{"static", staticErr, 0, false},
		{"mismatch", mismatchErr, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mounts := 0
			transient, err := startAnonymousImageNamespace(anonymousImageStartupValidatorStub{err: tc.err}, func() { mounts++ })
			if mounts != tc.wantMount || transient != tc.wantTransient || ((err != nil) != (tc.wantMount == 0)) {
				t.Fatalf("mounts/transient/error = %d/%v/%v", mounts, transient, err)
			}
		})
	}
}

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

func TestMappingBackendChangeRevokesUserSessions(t *testing.T) {
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "test",
	})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	defer app.Cleanup()
	registerMappingSessionRevocation(app)

	userID := createTestUser(t, app)
	otherUserID := createTestUserWithName(t, app, "bob", "gateway-bob")
	accountID := createTestBackendAccount(t, app)
	newAccountID := createTestBackendAccount(t, app)
	mapping := createTestMapping(t, app, userID, accountID, true)
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	createGatewaySession(t, app, userID, accountID, "active-1", now.Add(time.Hour), nil)
	createGatewaySession(t, app, userID, accountID, "active-2", now.Add(time.Hour), nil)
	revokedAt := now.Add(-time.Hour)
	createGatewaySession(t, app, userID, accountID, "already-revoked", now.Add(time.Hour), &revokedAt)
	createGatewaySession(t, app, otherUserID, accountID, "other-active", now.Add(time.Hour), nil)

	mapping.Set("backend_account", newAccountID)
	if err := app.Save(mapping); err != nil {
		t.Fatalf("save mapping: %v", err)
	}

	assertSessionRevoked(t, app, "active-1", true)
	assertSessionRevoked(t, app, "active-2", true)
	assertSessionRevoked(t, app, "already-revoked", true)
	assertSessionRevoked(t, app, "other-active", false)
	assertAuditEvent(t, app, userID, "sessions_revoked")
}

func TestMappingDisableRevokesUserSessions(t *testing.T) {
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "test",
	})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	defer app.Cleanup()
	registerMappingSessionRevocation(app)

	userID := createTestUser(t, app)
	accountID := createTestBackendAccount(t, app)
	mapping := createTestMapping(t, app, userID, accountID, true)
	createGatewaySession(t, app, userID, accountID, "active", time.Now().UTC().Add(time.Hour), nil)

	mapping.Set("enabled", false)
	if err := app.Save(mapping); err != nil {
		t.Fatalf("save mapping: %v", err)
	}

	assertSessionRevoked(t, app, "active", true)
	assertAuditEvent(t, app, userID, "sessions_revoked")
}

func TestMappingUnrelatedUpdateKeepsUserSessions(t *testing.T) {
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "test",
	})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	defer app.Cleanup()
	registerMappingSessionRevocation(app)

	userID := createTestUser(t, app)
	accountID := createTestBackendAccount(t, app)
	mapping := createTestMapping(t, app, userID, accountID, true)
	createGatewaySession(t, app, userID, accountID, "active", time.Now().UTC().Add(time.Hour), nil)

	mapping.Set("enabled", true)
	if err := app.Save(mapping); err != nil {
		t.Fatalf("save mapping: %v", err)
	}

	assertSessionRevoked(t, app, "active", false)
	assertNoAuditEvent(t, app, userID, "sessions_revoked")
}

func createTestUser(t *testing.T, app core.App) string {
	t.Helper()
	return createTestUserWithName(t, app, "alice", "gateway-user")
}

func createTestUserWithName(t *testing.T, app core.App, username, syntheticUserID string) string {
	t.Helper()
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users: %v", err)
	}
	record := core.NewRecord(users)
	record.Set("username", username)
	record.Set("email", username+"@example.com")
	record.Set("synthetic_user_id", syntheticUserID)
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

func createTestMapping(t *testing.T, app core.App, userID, accountID string, enabled bool) *core.Record {
	t.Helper()
	mappings, err := app.FindCollectionByNameOrId("user_mappings")
	if err != nil {
		t.Fatalf("find user_mappings: %v", err)
	}
	record := core.NewRecord(mappings)
	record.Set("gateway_user", userID)
	record.Set("backend_account", accountID)
	record.Set("enabled", enabled)
	if err := app.Save(record); err != nil {
		t.Fatalf("save mapping: %v", err)
	}
	return record
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

func assertSessionRevoked(t *testing.T, app core.App, tokenHash string, wantRevoked bool) {
	t.Helper()
	record, err := app.FindFirstRecordByData("gateway_sessions", "gateway_token_hash", tokenHash)
	if err != nil {
		t.Fatalf("find session %q: %v", tokenHash, err)
	}
	revoked := !record.GetDateTime("revoked_at").IsZero()
	if revoked != wantRevoked {
		t.Fatalf("session %q revoked=%v, want %v", tokenHash, revoked, wantRevoked)
	}
}

func assertAuditEvent(t *testing.T, app core.App, userID, event string) {
	t.Helper()
	if !hasAuditEvent(t, app, userID, event) {
		t.Fatalf("missing audit event %q for user %s", event, userID)
	}
}

func assertNoAuditEvent(t *testing.T, app core.App, userID, event string) {
	t.Helper()
	if hasAuditEvent(t, app, userID, event) {
		t.Fatalf("unexpected audit event %q for user %s", event, userID)
	}
}

func hasAuditEvent(t *testing.T, app core.App, userID, event string) bool {
	t.Helper()
	records, err := app.FindRecordsByFilter("audit_logs", "gateway_user = {:userID} && event = {:event}", "", 1, 0, map[string]any{"userID": userID, "event": event})
	if err != nil {
		t.Fatalf("find audit logs: %v", err)
	}
	return len(records) > 0
}
