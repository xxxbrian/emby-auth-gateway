package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
	"github.com/xxxbrian/emby-auth-gateway/internal/sessionid"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

func newProductionGatewayApp(t *testing.T) *pocketbase.PocketBase {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	app := newGatewayApp()
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestProductionBootstrapInitializesAndValidatesSchema(t *testing.T) {
	app := newProductionGatewayApp(t)
	if _, err := app.FindCollectionByNameOrId("upstream_endpoints"); err != nil {
		t.Fatalf("fresh bootstrap did not initialize schema: %v", err)
	}
	if err := app.ResetBootstrapState(); err != nil {
		t.Fatalf("reset fresh app: %v", err)
	}

	reopened := newGatewayApp()
	if err := reopened.Bootstrap(); err != nil {
		t.Fatalf("repeated bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = reopened.ResetBootstrapState() })
	if _, err := reopened.FindCollectionByNameOrId("upstream_endpoints"); err != nil {
		t.Fatalf("repeated bootstrap lost schema: %v", err)
	}
}

func TestProductionBootstrapRejectsPartialSchemaWithoutRepair(t *testing.T) {
	app := newProductionGatewayApp(t)
	collection, err := app.FindCollectionByNameOrId("upstream_endpoints")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Delete(collection); err != nil {
		t.Fatalf("delete fixture collection: %v", err)
	}
	if err := app.ResetBootstrapState(); err != nil {
		t.Fatalf("reset fixture app: %v", err)
	}

	reopened := newGatewayApp()
	if err := reopened.Bootstrap(); err == nil {
		t.Fatal("partial schema bootstrap succeeded")
	}
	_ = reopened.ResetBootstrapState()

	checker := pocketbase.New()
	if err := checker.Bootstrap(); err != nil {
		t.Fatalf("bootstrap checker: %v", err)
	}
	t.Cleanup(func() { _ = checker.ResetBootstrapState() })
	if _, err := checker.FindCollectionByNameOrId("upstream_endpoints"); err == nil {
		t.Fatal("partial schema was repaired")
	}
}

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

func TestCleanupPlaybackEventsKeepsOnlyRecentEvents(t *testing.T) {
	app := newProductionGatewayApp(t)

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
	app := newProductionGatewayApp(t)

	userID := createTestUser(t, app)
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	createGatewaySession(t, app, userID, "expired-old", now.Add(-8*24*time.Hour), nil)
	revokedOld := now.Add(-8 * 24 * time.Hour)
	createGatewaySession(t, app, userID, "revoked-old", now.Add(24*time.Hour), &revokedOld)
	revokedRecent := now.Add(-6 * 24 * time.Hour)
	createGatewaySession(t, app, userID, "revoked-recent", now.Add(24*time.Hour), &revokedRecent)
	createGatewaySession(t, app, userID, "active", now.Add(24*time.Hour), nil)

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

func TestCleanupGatewaySessionsDeletesProfilesWithParentsAndPreservesActive(t *testing.T) {
	app := newProductionGatewayApp(t)
	ensureSessionProfilesCollection(t, app)

	userID := createTestUser(t, app)
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	expiredID := createGatewaySession(t, app, userID, "expired-old", now.Add(-8*24*time.Hour), nil)
	activeID := createGatewaySession(t, app, userID, "active", now.Add(24*time.Hour), nil)
	createGatewaySessionProfile(t, app, expiredID, "session-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", now.Add(-8*24*time.Hour))
	createGatewaySessionProfile(t, app, activeID, "session-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", now)

	if err := cleanupGatewaySessions(app, now); err != nil {
		t.Fatalf("cleanup gateway sessions: %v", err)
	}

	sessions, err := app.FindAllRecords("gateway_sessions")
	if err != nil {
		t.Fatalf("query gateway sessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Id != activeID {
		t.Fatalf("remaining sessions = %#v, want only active %q", sessions, activeID)
	}

	profiles, err := app.FindAllRecords("gateway_session_profiles")
	if err != nil {
		t.Fatalf("query gateway_session_profiles: %v", err)
	}
	if len(profiles) != 1 || profiles[0].GetString("gateway_session") != activeID {
		t.Fatalf("remaining profiles = %#v, want only active profile", profiles)
	}
	if profiles[0].GetString("public_session_id") != "session-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("active profile public id = %q", profiles[0].GetString("public_session_id"))
	}
}

func TestCleanupGatewaySessionsPurgesOrphanProfiles(t *testing.T) {
	app := newProductionGatewayApp(t)
	ensureSessionProfilesCollection(t, app)

	userID := createTestUser(t, app)
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	activeID := createGatewaySession(t, app, userID, "active", now.Add(24*time.Hour), nil)
	createGatewaySessionProfile(t, app, activeID, "session-cccccccccccccccccccccccccccccccc", now)
	insertOrphanSessionProfile(t, app, "missing-session-id", "session-dddddddddddddddddddddddddddddddd", now.Add(-30*24*time.Hour))

	if err := cleanupGatewaySessions(app, now); err != nil {
		t.Fatalf("cleanup gateway sessions: %v", err)
	}

	profiles, err := app.FindAllRecords("gateway_session_profiles")
	if err != nil {
		t.Fatalf("query gateway_session_profiles: %v", err)
	}
	if len(profiles) != 1 || profiles[0].GetString("gateway_session") != activeID {
		t.Fatalf("remaining profiles = %#v, want only active profile", profiles)
	}
}

func TestCleanupGatewaySessionsCompatibleWhenSidecarAbsent(t *testing.T) {
	app := newProductionGatewayApp(t)
	// Simulate old/pre-migration binary: physical sidecar table is gone.
	if _, err := app.DB().NewQuery(`drop table if exists gateway_session_profiles`).Execute(); err != nil {
		t.Fatalf("drop gateway_session_profiles: %v", err)
	}
	if app.HasTable("gateway_session_profiles") {
		t.Fatal("expected gateway_session_profiles table to be absent")
	}

	userID := createTestUser(t, app)
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	createGatewaySession(t, app, userID, "expired-old", now.Add(-8*24*time.Hour), nil)
	createGatewaySession(t, app, userID, "active", now.Add(24*time.Hour), nil)

	if err := cleanupGatewaySessions(app, now); err != nil {
		t.Fatalf("cleanup without sidecar table: %v", err)
	}

	records, err := app.FindAllRecords("gateway_sessions")
	if err != nil {
		t.Fatalf("query gateway sessions: %v", err)
	}
	if len(records) != 1 || records[0].GetString("gateway_token_hash") != "active" {
		t.Fatalf("remaining gateway sessions = %#v, want only active", records)
	}
}

func TestCleanupGatewaySessionsTransactionFailureRollsBack(t *testing.T) {
	app := newProductionGatewayApp(t)
	ensureSessionProfilesCollection(t, app)

	userID := createTestUser(t, app)
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	expiredID := createGatewaySession(t, app, userID, "expired-old", now.Add(-8*24*time.Hour), nil)
	createGatewaySessionProfile(t, app, expiredID, "session-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", now.Add(-8*24*time.Hour))

	// Block parent deletion so the transaction fails after profile deletes begin.
	if _, err := app.DB().NewQuery(`
create table cleanup_fk_block (
	id text primary key not null,
	session_id text not null references gateway_sessions(id)
)`).Execute(); err != nil {
		t.Fatalf("create fk block table: %v", err)
	}
	if _, err := app.DB().NewQuery(`
insert into cleanup_fk_block (id, session_id) values ('block', {:session})
`).Bind(map[string]any{"session": expiredID}).Execute(); err != nil {
		t.Fatalf("seed fk block: %v", err)
	}

	if err := cleanupGatewaySessions(app, now); err == nil {
		t.Fatal("cleanup expected to fail under foreign-key block")
	}

	sessions, err := app.FindAllRecords("gateway_sessions")
	if err != nil {
		t.Fatalf("query gateway sessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Id != expiredID {
		t.Fatalf("sessions after failed cleanup = %#v, want rolled-back parent", sessions)
	}
	profiles, err := app.FindAllRecords("gateway_session_profiles")
	if err != nil {
		t.Fatalf("query gateway_session_profiles: %v", err)
	}
	if len(profiles) != 1 || profiles[0].GetString("gateway_session") != expiredID {
		t.Fatalf("profiles after failed cleanup = %#v, want rolled-back profile", profiles)
	}
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

func createGatewaySession(t *testing.T, app core.App, userID, tokenHash string, expiresAt time.Time, revokedAt *time.Time) string {
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
	record.Set("expires_at", expiresAt)
	if revokedAt != nil {
		record.Set("revoked_at", *revokedAt)
	}
	if err := app.Save(record); err != nil {
		t.Fatalf("save gateway session: %v", err)
	}
	return record.Id
}

func ensureSessionProfilesCollection(t *testing.T, app core.App) *core.Collection {
	t.Helper()
	if collection, err := app.FindCollectionByNameOrId(pbschema.SessionProfilesCollection); err == nil {
		return collection
	}
	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatalf("find gateway_sessions: %v", err)
	}
	if err := app.Save(pbschema.SessionProfiles(sessions.Id)); err != nil {
		t.Fatalf("save gateway_session_profiles collection: %v", err)
	}
	persisted, err := app.FindCollectionByNameOrId(pbschema.SessionProfilesCollection)
	if err != nil {
		t.Fatalf("reload gateway_session_profiles: %v", err)
	}
	return persisted
}

func createGatewaySessionProfile(t *testing.T, app core.App, sessionID, publicSessionID string, lastActivityAt time.Time) string {
	t.Helper()
	if !sessionid.Valid(publicSessionID) {
		t.Fatalf("invalid test public session id %q", publicSessionID)
	}
	profiles := ensureSessionProfilesCollection(t, app)
	record := core.NewRecord(profiles)
	record.Set("gateway_session", sessionID)
	record.Set("public_session_id", publicSessionID)
	record.Set("capabilities_json", "{}")
	record.Set("last_activity_at", lastActivityAt)
	if err := app.Save(record); err != nil {
		t.Fatalf("save gateway session profile: %v", err)
	}
	return record.Id
}

func insertOrphanSessionProfile(t *testing.T, app core.App, missingSessionID, publicSessionID string, lastActivityAt time.Time) {
	t.Helper()
	if !sessionid.Valid(publicSessionID) {
		t.Fatalf("invalid test public session id %q", publicSessionID)
	}
	ensureSessionProfilesCollection(t, app)
	// Bypass relation validation so we can simulate old-binary leftovers.
	if _, err := app.DB().NewQuery(`
insert into gateway_session_profiles (id, gateway_session, public_session_id, capabilities_json, last_activity_at, created, updated)
values ({:id}, {:session}, {:public_id}, '{}', {:activity}, {:activity}, {:activity})
`).Bind(map[string]any{
		"id":        "orphanprofile01",
		"session":   missingSessionID,
		"public_id": publicSessionID,
		"activity":  lastActivityAt.UTC().Format("2006-01-02 15:04:05.000Z"),
	}).Execute(); err != nil {
		t.Fatalf("insert orphan profile: %v", err)
	}
}
