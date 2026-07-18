package pbmigrations

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
)

// applyCurrentPlaybacksMigration runs only the Phase 4 up via a private
// MigrationsList. Predecessor history must already exist (or the up fails).
func applyCurrentPlaybacksMigration(app core.App) error {
	list := core.MigrationsList{}
	list.Register(upGatewayCurrentPlaybacks, downGatewayCurrentPlaybacks, migrationGatewayCurrentPlaybacks)
	return applyPrivate(app, list, validateExtensions)
}

// preparePhase3Base ensures base schema + Phase 3 session-profiles migration
// history so Phase 4 predecessor checks succeed.
func preparePhase3Base(t *testing.T) core.App {
	t.Helper()
	app := newBareApp(t)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatal(err)
	}
	if err := applySessionProfilesMigration(app); err != nil {
		t.Fatalf("phase 3 migrate: %v", err)
	}
	assertMigrationHistory(t, app, migrationGatewaySessionProfiles, true)
	assertMigrationHistory(t, app, migrationGatewayCurrentPlaybacks, false)
	return app
}

func TestCurrentPlaybacksUpRequiresPhase3Predecessor(t *testing.T) {
	app := newBareApp(t)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatal(err)
	}
	// Exact session-profiles collection without Phase 3 history must still fail
	// the predecessor gate before any current-playbacks mutation.
	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Save(pbschema.SessionProfiles(sessions.Id)); err != nil {
		t.Fatal(err)
	}
	assertMigrationHistory(t, app, migrationGatewaySessionProfiles, false)

	err = applyCurrentPlaybacksMigration(app)
	if err == nil {
		t.Fatal("expected predecessor failure")
	}
	if !strings.Contains(err.Error(), "predecessor") || !strings.Contains(err.Error(), migrationGatewaySessionProfiles) {
		t.Fatalf("error = %v, want exact predecessor %q", err, migrationGatewaySessionProfiles)
	}
	assertMigrationHistory(t, app, migrationGatewayCurrentPlaybacks, false)
	if _, findErr := app.FindCollectionByNameOrId(pbschema.CurrentPlaybacksCollection); findErr == nil {
		t.Fatal("current playbacks collection created without predecessor history")
	}
}

func TestCurrentPlaybacksUpAdoptsExactEmptyCollection(t *testing.T) {
	app := preparePhase3Base(t)
	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Save(pbschema.CurrentPlaybacks(sessions.Id)); err != nil {
		t.Fatal(err)
	}
	if err := pbschema.ValidateCurrentPlaybacks(app); err != nil {
		t.Fatalf("pre-migrate exact empty: %v", err)
	}
	if countCurrentPlaybacks(t, app) != 0 {
		t.Fatal("pre-seed must be empty")
	}

	var creates, updates atomic.Int32
	createHook := app.OnCollectionCreate().BindFunc(func(e *core.CollectionEvent) error {
		if e.Collection != nil && e.Collection.Name == pbschema.CurrentPlaybacksCollection {
			creates.Add(1)
		}
		return e.Next()
	})
	updateHook := app.OnCollectionUpdate().BindFunc(func(e *core.CollectionEvent) error {
		if e.Collection != nil && e.Collection.Name == pbschema.CurrentPlaybacksCollection {
			updates.Add(1)
		}
		return e.Next()
	})
	defer app.OnCollectionCreate().Unbind(createHook)
	defer app.OnCollectionUpdate().Unbind(updateHook)

	if err := applyCurrentPlaybacksMigration(app); err != nil {
		t.Fatalf("adopt exact empty: %v", err)
	}
	assertMigrationHistory(t, app, migrationGatewayCurrentPlaybacks, true)
	if err := pbschema.ValidateCurrentPlaybacks(app); err != nil {
		t.Fatalf("post-adopt validate: %v", err)
	}
	if countCurrentPlaybacks(t, app) != 0 {
		t.Fatal("adoption must leave zero rows")
	}
	if creates.Load() != 0 {
		t.Fatalf("adoption recreated collection (%d creates)", creates.Load())
	}
	if updates.Load() != 0 {
		t.Fatalf("adoption rewrote collection (%d updates)", updates.Load())
	}

	// Repeat is native history no-op + write-free final validation.
	if err := applyCurrentPlaybacksMigration(app); err != nil {
		t.Fatalf("second adopt/history no-op: %v", err)
	}
	if creates.Load() != 0 || updates.Load() != 0 {
		t.Fatalf("second run mutated collection creates=%d updates=%d", creates.Load(), updates.Load())
	}
	if countCurrentPlaybacks(t, app) != 0 {
		t.Fatal("second run introduced rows")
	}
}

func TestCurrentPlaybacksUpRejectsDriftedCollectionAtomically(t *testing.T) {
	app := preparePhase3Base(t)
	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Save(pbschema.CurrentPlaybacks(sessions.Id)); err != nil {
		t.Fatal(err)
	}
	c, err := app.FindCollectionByNameOrId(pbschema.CurrentPlaybacksCollection)
	if err != nil {
		t.Fatal(err)
	}
	const driftedMax = 81
	c.Fields.GetByName("item_id").(*core.TextField).Max = driftedMax
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}

	err = applyCurrentPlaybacksMigration(app)
	if err == nil {
		t.Fatal("expected drifted collection rejection")
	}
	assertMigrationHistory(t, app, migrationGatewayCurrentPlaybacks, false)

	// Atomic: drifted shape preserved (no partial "repair").
	persisted, err := app.FindCollectionByNameOrId(pbschema.CurrentPlaybacksCollection)
	if err != nil {
		t.Fatal(err)
	}
	gotMax := persisted.Fields.GetByName("item_id").(*core.TextField).Max
	if gotMax != driftedMax {
		t.Fatalf("item_id.Max = %d after failed migrate, want drifted %d", gotMax, driftedMax)
	}
	if err := pbschema.ValidateCurrentPlaybacks(app); err == nil {
		t.Fatal("expected drifted schema to remain invalid")
	}
}

func TestCurrentPlaybacksUpRejectsPreexistingRowNoBackfill(t *testing.T) {
	app := preparePhase3Base(t)
	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Save(pbschema.CurrentPlaybacks(sessions.Id)); err != nil {
		t.Fatal(err)
	}

	userID := saveUser(t, app, "cp-row", "syn-cp-row")
	sessionID := saveSession(t, app, userID, "hash-cp-row")
	// Ensure profile coverage for Phase 3 sidecar (runtime-shaped fixture).
	if _, err := app.FindFirstRecordByData(pbschema.SessionProfilesCollection, "gateway_session", sessionID); err != nil {
		saveProfile(t, app, sessionID, mustSessionID(t), "{}")
	}

	// Legacy playback_events row that must never be synthesized into current.
	eventID := savePlaybackEvent(t, app, userID, "legacy-item-from-events")
	beforeEvents := countPlaybackEvents(t, app)
	if beforeEvents != 1 {
		t.Fatalf("playback_events seed count = %d", beforeEvents)
	}

	rowID := saveCurrentPlaybackRow(t, app, sessionID, "preexisting-item")
	if countCurrentPlaybacks(t, app) != 1 {
		t.Fatal("expected one preexisting current playback row")
	}

	err = applyCurrentPlaybacksMigration(app)
	if err == nil {
		t.Fatal("expected preexisting row rejection")
	}
	msg := err.Error()
	if !strings.Contains(msg, "preexisting") && !strings.Contains(msg, "row") {
		t.Fatalf("error = %v, want preexisting row rejection", err)
	}
	if !strings.Contains(msg, "no backfill") {
		t.Fatalf("error = %v, want no-backfill guidance", err)
	}
	assertMigrationHistory(t, app, migrationGatewayCurrentPlaybacks, false)

	// Row preserved exactly; no additional synthesis.
	if countCurrentPlaybacks(t, app) != 1 {
		t.Fatalf("current rows after fail = %d, want preserved 1", countCurrentPlaybacks(t, app))
	}
	row, err := app.FindRecordById(pbschema.CurrentPlaybacksCollection, rowID)
	if err != nil {
		t.Fatalf("preexisting row missing after fail: %v", err)
	}
	if row.GetString("item_id") != "preexisting-item" {
		t.Fatalf("item_id = %q, want preexisting-item", row.GetString("item_id"))
	}

	// playback_events untouched and not used as a backfill source.
	if countPlaybackEvents(t, app) != beforeEvents {
		t.Fatalf("playback_events count changed: %d -> %d", beforeEvents, countPlaybackEvents(t, app))
	}
	event, err := app.FindRecordById("playback_events", eventID)
	if err != nil {
		t.Fatalf("playback_events row missing: %v", err)
	}
	if event.GetString("item_id") != "legacy-item-from-events" {
		t.Fatalf("playback_events item_id = %q", event.GetString("item_id"))
	}
	// No second current row keyed from the legacy event item.
	for _, rec := range mustListCurrentPlaybacks(t, app) {
		if rec.GetString("item_id") == "legacy-item-from-events" {
			t.Fatal("migration synthesized current row from playback_events")
		}
	}
}

func TestCurrentPlaybacksDownUnsupported(t *testing.T) {
	err := downGatewayCurrentPlaybacks(nil)
	if err == nil {
		t.Fatal("expected unsupported down")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unsupported") {
		t.Fatalf("error = %v, want unsupported", err)
	}
	if !strings.Contains(msg, "pb_data") && !strings.Contains(msg, "backup") {
		t.Fatalf("error = %v, want backup/restore guidance", err)
	}
	if !strings.Contains(msg, migrationGatewayCurrentPlaybacks) {
		t.Fatalf("error = %v, want migration filename", err)
	}
}

func saveCurrentPlaybackRow(t *testing.T, app core.App, sessionID, itemID string) string {
	t.Helper()
	col, err := app.FindCollectionByNameOrId(pbschema.CurrentPlaybacksCollection)
	if err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(col)
	record.Set("gateway_session", sessionID)
	record.Set("item_id", itemID)
	record.Set("item_snapshot_json", "{}")
	record.Set("play_state_json", "{}")
	record.Set("started_at", time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC))
	record.Set("last_reported_at", time.Date(2030, 1, 2, 3, 5, 5, 0, time.UTC))
	if err := app.Save(record); err != nil {
		t.Fatalf("save current playback: %v", err)
	}
	return record.Id
}

func savePlaybackEvent(t *testing.T, app core.App, userID, itemID string) string {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("playback_events")
	if err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(col)
	record.Set("gateway_user", userID)
	record.Set("synthetic_user_id", "syn")
	record.Set("item_id", itemID)
	record.Set("event", "progress")
	record.Set("occurred_at", time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC))
	if err := app.Save(record); err != nil {
		t.Fatalf("save playback_events: %v", err)
	}
	return record.Id
}

func countPlaybackEvents(t *testing.T, app core.App) int {
	t.Helper()
	records, err := app.FindAllRecords("playback_events")
	if err != nil {
		t.Fatal(err)
	}
	return len(records)
}

func mustListCurrentPlaybacks(t *testing.T, app core.App) []*core.Record {
	t.Helper()
	records, err := app.FindAllRecords(pbschema.CurrentPlaybacksCollection)
	if err != nil {
		t.Fatal(err)
	}
	return records
}
