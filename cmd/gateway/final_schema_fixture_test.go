package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
	"github.com/xxxbrian/emby-auth-gateway/internal/sessionid"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// v060DataFixture was created from v0.6.0 tag 39df68c7f2dd19c8a08cb4828ffdb577f6a45231
// with PocketBase v0.39.6: real application migrations, canonical collection
// allowlist cleanup, generic application bookkeeping rows, and VACUUM.
//
//go:embed testdata/v060-final/v060-final.fixture
var v060Fixture []byte

const v060FixtureSHA256 = "c52199d57cf955616be85421738b07da2f1d65e46b556f59e766f4b02cbd2c9f"

func TestProductionBootstrapMigratesFrozenV060ThenIsWriteFree(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	if got := fixtureSHA256(v060Fixture); got != v060FixtureSHA256 {
		t.Fatalf("fixture sha256 = %s", got)
	}
	dataDir := "pb_data"
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "data.db"), v060Fixture, 0o600); err != nil {
		t.Fatal(err)
	}

	seed := pocketbase.New()
	if err := seed.Bootstrap(); err != nil {
		t.Fatalf("bootstrap fixture database: %v", err)
	}
	assertV060ApplicationCollectionSet(t, seed)
	seedFrozenState(t, seed)
	seedMarkers := captureSeededApplicationMarkers(t, seed)
	if err := seed.ResetBootstrapState(); err != nil {
		t.Fatalf("reset fixture database: %v", err)
	}

	// Stage 1: first current-binary bootstrap is allowed to apply additive
	// gateway_session_profiles (backfill) and gateway_current_playbacks (empty create).
	app := newGatewayApp()
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("production bootstrap frozen schema: %v", err)
	}
	assertSeededApplicationDataPreserved(t, app, seedMarkers)
	assertSessionProfilesBackfilled(t, app, seedMarkers.sessionIDs)
	assertCurrentPlaybacksCreatedEmpty(t, app)
	assertCurrentApplicationCollectionSet(t, app)
	assertSessionProfilesLockedAndDefaults(t, app)
	assertCurrentPlaybacksLocked(t, app)
	assertFixtureIntegrity(t, app)
	afterMigration := fixtureFingerprint(t, app)
	if err := app.ResetBootstrapState(); err != nil {
		t.Fatalf("reset after first bootstrap: %v", err)
	}

	// Stage 2: second bootstrap must be durable-fingerprint write-free.
	reopened := newGatewayApp()
	if err := reopened.Bootstrap(); err != nil {
		t.Fatalf("second production bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = reopened.ResetBootstrapState() })
	if after := fixtureFingerprint(t, reopened); after != afterMigration {
		t.Fatal("second production bootstrap changed durable state after migration")
	}
	assertFixtureIntegrity(t, reopened)

	// Stage 3: old-binary base validator accepts the upgraded DB write-free,
	// including the exact extra current-playback sidecar (outside requiredNames).
	beforeEnsure := fixtureFingerprint(t, reopened)
	if err := pbschema.Ensure(reopened); err != nil {
		t.Fatalf("old-base pbschema.Ensure on upgraded DB: %v", err)
	}
	if after := fixtureFingerprint(t, reopened); after != beforeEnsure {
		t.Fatal("old-base pbschema.Ensure wrote upgraded durable state")
	}
	// Sidecar remains exact and empty after write-free Ensure.
	assertCurrentPlaybacksCreatedEmpty(t, reopened)
}

func seedFrozenState(t *testing.T, app core.App) {
	t.Helper()
	first := core.NewBaseCollection("fixture_a")
	if err := app.Save(first); err != nil {
		t.Fatalf("save first extra collection: %v", err)
	}
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	second := core.NewBaseCollection("fixture_b")
	second.Fields.Add(&core.RelationField{Name: "first", CollectionId: first.Id, Required: true, MaxSelect: 1})
	second.Fields.Add(&core.RelationField{Name: "gateway_user", CollectionId: users.Id, Required: true, MaxSelect: 1})
	second.AddIndex("idx_fixture_b_first_user", true, "first, gateway_user", "")
	if err := app.Save(second); err != nil {
		t.Fatalf("save related extra collection: %v", err)
	}
	if _, err := app.DB().NewQuery("CREATE TABLE fixture_raw (id TEXT PRIMARY KEY, value TEXT NOT NULL)").Execute(); err != nil {
		t.Fatalf("create extra table: %v", err)
	}
	if _, err := app.DB().NewQuery("INSERT INTO fixture_raw (id, value) VALUES ('raw-row', 'retained')").Execute(); err != nil {
		t.Fatalf("seed extra table: %v", err)
	}

	user := fixtureRecord(t, app, "users", map[string]any{"id": "fixtureuser0001", "username": "fixture-user", "email": "fixture@example.test", "synthetic_user_id": "fixture-user", "enabled": true})
	user.SetPassword("fixture-password")
	saveFixtureRecord(t, app, user)
	saveFixtureRecord(t, app, fixtureRecord(t, app, "fixture_a", map[string]any{"id": "fixturefirst001"}))
	firstRecord, err := app.FindRecordById("fixture_a", "fixturefirst001")
	if err != nil {
		t.Fatal(err)
	}
	saveFixtureRecord(t, app, fixtureRecord(t, app, "fixture_b", map[string]any{"id": "fixturesecond01", "first": firstRecord.Id, "gateway_user": user.Id}))
	source := fixtureRecord(t, app, "upstream_sources", map[string]any{
		"id": "fixturesource01", "key": "default", "server_id": "fixture-server", "backend_username": "backend", "backend_password": "secret",
		"backend_user_agent": "fixture-agent", "backend_authorization_client": "fixture-client", "backend_authorization_device": "fixture-device", "backend_authorization_device_id": "fixture-device-id", "backend_authorization_version": "1",
	})
	saveFixtureRecord(t, app, source)
	saveFixtureRecord(t, app, fixtureRecord(t, app, "upstream_endpoints", map[string]any{"id": "fixtureendpoint", "source": source.Id, "key": "primary", "base_url": "https://fixture.example", "active": true}))
	saveFixtureRecord(t, app, fixtureRecord(t, app, "user_item_data", map[string]any{"id": "fixtureitemdata", "gateway_user": user.Id, "synthetic_user_id": "fixture-user", "item_id": "fixture-item", "item_name": "Fixture Item"}))
	saveFixtureRecord(t, app, fixtureRecord(t, app, "playback_events", map[string]any{"id": "fixtureplayback", "gateway_user": user.Id, "item_id": "fixture-item", "event": "progress", "occurred_at": time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}))
	saveFixtureRecord(t, app, fixtureRecord(t, app, "display_preferences", map[string]any{"id": "fixturedisplay1", "gateway_user": user.Id, "preference_id": "home", "client": "fixture", "payload_json": "{}"}))
	saveFixtureRecord(t, app, fixtureRecord(t, app, "audit_logs", map[string]any{"id": "fixtureauditlog", "gateway_user": user.Id, "event": "fixture"}))
	saveFixtureRecord(t, app, fixtureRecord(t, app, "gateway_sessions", map[string]any{"id": "fixturesession1", "gateway_token_hash": "fixture-token", "gateway_user": user.Id, "synthetic_user_id": "fixture-user", "expires_at": time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}))
	saveFixtureRecord(t, app, fixtureRecord(t, app, "item_child_counts", map[string]any{"id": "fixturecache001", "item_id": "fixture-item", "child_count": 3}))
}

type seededApplicationMarkers struct {
	userID     string
	sessionIDs []string
	recordIDs  map[string]string
	rawValue   string
}

func captureSeededApplicationMarkers(t *testing.T, app core.App) seededApplicationMarkers {
	t.Helper()
	markers := seededApplicationMarkers{
		userID: "fixtureuser0001",
		recordIDs: map[string]string{
			"upstream_sources":    "fixturesource01",
			"upstream_endpoints":  "fixtureendpoint",
			"user_item_data":      "fixtureitemdata",
			"playback_events":     "fixtureplayback",
			"display_preferences": "fixturedisplay1",
			"audit_logs":          "fixtureauditlog",
			"gateway_sessions":    "fixturesession1",
			"item_child_counts":   "fixturecache001",
		},
	}
	sessions, err := app.FindAllRecords("gateway_sessions")
	if err != nil {
		t.Fatalf("list seeded sessions: %v", err)
	}
	for _, session := range sessions {
		markers.sessionIDs = append(markers.sessionIDs, session.Id)
	}
	sort.Strings(markers.sessionIDs)
	if err := app.DB().NewQuery("SELECT value FROM fixture_raw WHERE id = 'raw-row'").Row(&markers.rawValue); err != nil {
		t.Fatalf("read fixture_raw: %v", err)
	}
	return markers
}

func assertSeededApplicationDataPreserved(t *testing.T, app core.App, markers seededApplicationMarkers) {
	t.Helper()
	if _, err := app.FindRecordById("users", markers.userID); err != nil {
		t.Fatalf("seeded user missing after migration: %v", err)
	}
	for collection, id := range markers.recordIDs {
		if _, err := app.FindRecordById(collection, id); err != nil {
			t.Fatalf("seeded %s/%s missing after migration: %v", collection, id, err)
		}
	}
	var rawValue string
	if err := app.DB().NewQuery("SELECT value FROM fixture_raw WHERE id = 'raw-row'").Row(&rawValue); err != nil {
		t.Fatalf("fixture_raw missing after migration: %v", err)
	}
	if rawValue != markers.rawValue {
		t.Fatalf("fixture_raw value = %q, want %q", rawValue, markers.rawValue)
	}
}

func assertSessionProfilesBackfilled(t *testing.T, app core.App, sessionIDs []string) {
	t.Helper()
	if !app.HasTable(pbschema.SessionProfilesCollection) {
		t.Fatal("expected gateway_session_profiles after migration bootstrap")
	}
	profiles, err := app.FindAllRecords(pbschema.SessionProfilesCollection)
	if err != nil {
		t.Fatalf("list gateway_session_profiles: %v", err)
	}
	bySession := map[string]*core.Record{}
	for _, profile := range profiles {
		bySession[profile.GetString("gateway_session")] = profile
	}
	if len(bySession) != len(sessionIDs) {
		t.Fatalf("backfilled profiles = %d, want %d for sessions %v", len(bySession), len(sessionIDs), sessionIDs)
	}
	for _, sessionID := range sessionIDs {
		profile := bySession[sessionID]
		if profile == nil {
			t.Fatalf("missing backfilled profile for session %q", sessionID)
		}
		publicID := profile.GetString("public_session_id")
		if !sessionid.Valid(publicID) {
			t.Fatalf("profile %q public_session_id = %q, want valid session-<32hex>", profile.Id, publicID)
		}
		if caps := profile.GetString("capabilities_json"); caps != "{}" {
			t.Fatalf("profile %q capabilities_json = %q, want {}", profile.Id, caps)
		}
		session, err := app.FindRecordById("gateway_sessions", sessionID)
		if err != nil {
			t.Fatalf("find session %q: %v", sessionID, err)
		}
		// Migration derives last_activity_at from the parent session created stamp.
		if !profile.GetDateTime("last_activity_at").Time().Equal(session.GetDateTime("created").Time()) {
			t.Fatalf("profile %q last_activity_at = %v, want session created %v",
				profile.Id, profile.GetDateTime("last_activity_at"), session.GetDateTime("created"))
		}
	}
}

func assertSessionProfilesLockedAndDefaults(t *testing.T, app core.App) {
	t.Helper()
	if err := pbschema.ValidateSessionProfiles(app); err != nil {
		t.Fatalf("gateway_session_profiles exact schema: %v", err)
	}
	collection, err := app.FindCollectionByNameOrId(pbschema.SessionProfilesCollection)
	if err != nil {
		t.Fatalf("find gateway_session_profiles: %v", err)
	}
	if collection.ListRule != nil || collection.ViewRule != nil || collection.CreateRule != nil || collection.UpdateRule != nil || collection.DeleteRule != nil {
		t.Fatalf("gateway_session_profiles rules are not fully locked: list=%v view=%v create=%v update=%v delete=%v",
			collection.ListRule, collection.ViewRule, collection.CreateRule, collection.UpdateRule, collection.DeleteRule)
	}
	profiles, err := app.FindAllRecords(pbschema.SessionProfilesCollection)
	if err != nil {
		t.Fatalf("list gateway_session_profiles: %v", err)
	}
	for _, profile := range profiles {
		if !sessionid.Valid(profile.GetString("public_session_id")) {
			t.Fatalf("profile %q public_session_id invalid: %q", profile.Id, profile.GetString("public_session_id"))
		}
		if profile.GetString("capabilities_json") != "{}" {
			t.Fatalf("profile %q capabilities_json = %q, want {}", profile.Id, profile.GetString("capabilities_json"))
		}
		if profile.GetDateTime("last_activity_at").IsZero() {
			t.Fatalf("profile %q last_activity_at is zero", profile.Id)
		}
	}
}

func assertCurrentPlaybacksCreatedEmpty(t *testing.T, app core.App) {
	t.Helper()
	if !app.HasTable(pbschema.CurrentPlaybacksCollection) {
		t.Fatal("expected gateway_current_playbacks after migration bootstrap")
	}
	if err := pbschema.ValidateCurrentPlaybacks(app); err != nil {
		t.Fatalf("gateway_current_playbacks exact schema: %v", err)
	}
	records, err := app.FindAllRecords(pbschema.CurrentPlaybacksCollection)
	if err != nil {
		t.Fatalf("list gateway_current_playbacks: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("gateway_current_playbacks rows = %d, want 0 (no backfill)", len(records))
	}
}

func assertCurrentPlaybacksLocked(t *testing.T, app core.App) {
	t.Helper()
	if err := pbschema.ValidateCurrentPlaybacks(app); err != nil {
		t.Fatalf("gateway_current_playbacks exact schema: %v", err)
	}
	collection, err := app.FindCollectionByNameOrId(pbschema.CurrentPlaybacksCollection)
	if err != nil {
		t.Fatalf("find gateway_current_playbacks: %v", err)
	}
	if collection.ListRule != nil || collection.ViewRule != nil || collection.CreateRule != nil || collection.UpdateRule != nil || collection.DeleteRule != nil {
		t.Fatalf("gateway_current_playbacks rules are not fully locked: list=%v view=%v create=%v update=%v delete=%v",
			collection.ListRule, collection.ViewRule, collection.CreateRule, collection.UpdateRule, collection.DeleteRule)
	}
}

func fixtureRecord(t *testing.T, app core.App, collectionName string, values map[string]any) *core.Record {
	t.Helper()
	collection, err := app.FindCollectionByNameOrId(collectionName)
	if err != nil {
		t.Fatalf("find %s: %v", collectionName, err)
	}
	record := core.NewRecord(collection)
	for key, value := range values {
		record.Set(key, value)
	}
	return record
}

func saveFixtureRecord(t *testing.T, app core.App, record *core.Record) {
	t.Helper()
	if err := app.Save(record); err != nil {
		t.Fatalf("save fixture record %q: %v", record.Id, err)
	}
}

func fixtureFingerprint(t *testing.T, app core.App) string {
	t.Helper()
	var tableNames string
	if err := app.DB().NewQuery("SELECT coalesce(group_concat(name, ','), '') FROM (SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name)").Row(&tableNames); err != nil {
		t.Fatal(err)
	}
	parts := make([]string, 0, 32)
	for _, table := range strings.Split(tableNames, ",") {
		if table == "" {
			continue
		}
		info, err := app.TableInfo(table)
		if err != nil {
			t.Fatal(err)
		}
		indexes, err := app.TableIndexes(table)
		if err != nil {
			t.Fatal(err)
		}
		physical, err := json.Marshal(struct{ Info, Indexes any }{info, indexes})
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, table, string(physical), rawTableRows(t, app, table, info))
	}
	for _, query := range []string{
		"SELECT coalesce(group_concat(entry, '\n'), '') FROM (SELECT id || ':' || name || ':' || fields || ':' || options AS entry FROM _collections ORDER BY id)",
		"SELECT coalesce(group_concat(entry, '\n'), '') FROM (SELECT type || ':' || name || ':' || coalesce(sql, '') AS entry FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name)",
		"SELECT coalesce(group_concat(entry, '\n'), '') FROM (SELECT file || ':' || applied AS entry FROM _migrations ORDER BY file)",
		"SELECT coalesce(group_concat(id || ':' || value, '\n'), '') FROM fixture_raw ORDER BY id",
	} {
		var value string
		if err := app.DB().NewQuery(query).Row(&value); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, value)
	}
	return fmt.Sprintf("%x", strings.Join(parts, "\n---\n"))
}

func rawTableRows(t *testing.T, app core.App, table string, info []*core.TableInfoRow) string {
	t.Helper()
	columns := make([]string, 0, len(info))
	order := make([]string, 0, len(info))
	for _, column := range info {
		quoted := quoteIdentifier(column.Name)
		columns = append(columns, "quote("+quoted+")")
		if column.PK > 0 {
			order = append(order, quoted)
		}
	}
	if len(order) == 0 {
		for _, column := range info {
			order = append(order, quoteIdentifier(column.Name))
		}
	}
	var rows string
	query := "SELECT coalesce(group_concat(row, char(10)), '') FROM (SELECT " + strings.Join(columns, " || '|' || ") + " AS row FROM " + quoteIdentifier(table) + " ORDER BY " + strings.Join(order, ", ") + ")"
	if err := app.DB().NewQuery(query).Row(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func fixtureSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func assertV060ApplicationCollectionSet(t *testing.T, app core.App) {
	t.Helper()
	assertApplicationCollectionSet(t, app, []string{
		"audit_logs", "display_preferences", "gateway_sessions", "item_child_counts", "path_policies",
		"playback_events", "upstream_endpoints", "upstream_sources", "user_item_data", "users",
	})
}

func assertCurrentApplicationCollectionSet(t *testing.T, app core.App) {
	t.Helper()
	assertApplicationCollectionSet(t, app, []string{
		"audit_logs", "display_preferences", "gateway_current_playbacks", "gateway_session_profiles", "gateway_sessions", "item_child_counts",
		"path_policies", "playback_events", "upstream_endpoints", "upstream_sources", "user_item_data", "users",
	})
}

func assertApplicationCollectionSet(t *testing.T, app core.App, want []string) {
	t.Helper()
	collections, err := app.FindAllCollections()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(want))
	for _, collection := range collections {
		if !collection.System {
			got = append(got, collection.Name)
		}
	}
	sort.Strings(got)
	// Fixture extras (fixture_a/fixture_b) are intentional and excluded from the
	// application allowlist comparison.
	filtered := make([]string, 0, len(got))
	for _, name := range got {
		if name == "fixture_a" || name == "fixture_b" {
			continue
		}
		filtered = append(filtered, name)
	}
	if strings.Join(filtered, ",") != strings.Join(want, ",") {
		t.Fatalf("application collections = %v, want %v", filtered, want)
	}
}

func assertFixtureIntegrity(t *testing.T, app core.App) {
	t.Helper()
	var integrity string
	if err := app.DB().NewQuery("PRAGMA integrity_check").Row(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q, %v", integrity, err)
	}
	var violations int
	if err := app.DB().NewQuery("SELECT count(*) FROM pragma_foreign_key_check").Row(&violations); err != nil || violations != 0 {
		t.Fatalf("foreign_key_check count = %d, %v", violations, err)
	}
}
