package pbschema

import (
	"errors"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func TestCurrentPlaybacksBuilderExactShape(t *testing.T) {
	t.Parallel()

	const sessionID = "pbc_sessions_test01"
	c := CurrentPlaybacks(sessionID)

	if c.Name != CurrentPlaybacksCollection {
		t.Fatalf("name = %q", c.Name)
	}
	if c.Type != core.CollectionTypeBase {
		t.Fatalf("type = %q", c.Type)
	}
	if c.ListRule != nil || c.ViewRule != nil || c.CreateRule != nil || c.UpdateRule != nil || c.DeleteRule != nil {
		t.Fatalf("API rules must all be nil: list=%v view=%v create=%v update=%v delete=%v",
			c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule)
	}

	// id is auto; then the eleven declared fields.
	wantNames := []string{
		"id",
		"gateway_session",
		"item_id",
		"play_session_id",
		"media_source_id",
		"item_snapshot_json",
		"play_state_json",
		"run_time_ticks",
		"started_at",
		"last_reported_at",
		"created",
		"updated",
	}
	if len(c.Fields) != len(wantNames) {
		t.Fatalf("field count = %d, want %d", len(c.Fields), len(wantNames))
	}
	for i, name := range wantNames {
		if c.Fields[i].GetName() != name {
			t.Fatalf("field[%d] = %q, want %q", i, c.Fields[i].GetName(), name)
		}
	}

	rel := c.Fields.GetByName("gateway_session").(*core.RelationField)
	if !rel.Required || rel.MaxSelect != 1 || !rel.CascadeDelete || rel.CollectionId != sessionID {
		t.Fatalf("gateway_session relation = %#v", rel)
	}

	itemID := c.Fields.GetByName("item_id").(*core.TextField)
	if !itemID.Required || itemID.Min != 1 || itemID.Max != 80 {
		t.Fatalf("item_id = %#v", itemID)
	}

	playSessionID := c.Fields.GetByName("play_session_id").(*core.TextField)
	if playSessionID.Required || playSessionID.Max != 255 {
		t.Fatalf("play_session_id = %#v", playSessionID)
	}

	mediaSourceID := c.Fields.GetByName("media_source_id").(*core.TextField)
	if mediaSourceID.Required || mediaSourceID.Max != 255 {
		t.Fatalf("media_source_id = %#v", mediaSourceID)
	}

	itemSnapshot := c.Fields.GetByName("item_snapshot_json").(*core.TextField)
	if !itemSnapshot.Required || itemSnapshot.Min != 2 || itemSnapshot.Max != 65536 {
		t.Fatalf("item_snapshot_json = %#v", itemSnapshot)
	}

	playState := c.Fields.GetByName("play_state_json").(*core.TextField)
	if !playState.Required || playState.Min != 2 || playState.Max != 16384 {
		t.Fatalf("play_state_json = %#v", playState)
	}

	runTime := c.Fields.GetByName("run_time_ticks").(*core.NumberField)
	if runTime.Required || !runTime.OnlyInt {
		t.Fatalf("run_time_ticks = %#v", runTime)
	}

	startedAt := c.Fields.GetByName("started_at").(*core.DateField)
	if !startedAt.Required {
		t.Fatal("started_at must be required")
	}
	lastReportedAt := c.Fields.GetByName("last_reported_at").(*core.DateField)
	if !lastReportedAt.Required {
		t.Fatal("last_reported_at must be required")
	}

	created := c.Fields.GetByName("created").(*core.AutodateField)
	if !created.OnCreate || created.OnUpdate {
		t.Fatalf("created autodate = %#v", created)
	}
	updated := c.Fields.GetByName("updated").(*core.AutodateField)
	if !updated.OnCreate || !updated.OnUpdate {
		t.Fatalf("updated autodate = %#v", updated)
	}

	wantIndexes := []string{
		`CREATE UNIQUE INDEX idx_gateway_current_playbacks_session ON gateway_current_playbacks (gateway_session)`,
	}
	// PocketBase AddIndex stores SQL; compare via indexesEqual normalization path.
	if !indexesEqual(c.Indexes, wantIndexes) {
		// Accept either quoted or unquoted forms by checking names present.
		gotNames := map[string]bool{}
		for _, idx := range c.Indexes {
			if strings.Contains(idx, "idx_gateway_current_playbacks_session") {
				gotNames["session"] = true
			}
			if !strings.Contains(strings.ToUpper(idx), "UNIQUE") {
				t.Fatalf("index is not unique: %s", idx)
			}
		}
		if len(c.Indexes) != 1 || !gotNames["session"] {
			t.Fatalf("indexes = %#v", c.Indexes)
		}
	}
}

func TestCurrentPlaybacksNotInBaseRequiredOrEnsureCreateSet(t *testing.T) {
	t.Parallel()

	for _, name := range requiredNames {
		if name == CurrentPlaybacksCollection {
			t.Fatal("CurrentPlaybacksCollection must not be in requiredNames")
		}
	}
	if requiredCollectionName(CurrentPlaybacksCollection) {
		t.Fatal("requiredCollectionName must not treat sidecar as base-required")
	}

	// Fresh Ensure must not create the sidecar.
	app := testApp(t)
	if err := Ensure(app); err != nil {
		t.Fatal(err)
	}
	if _, err := app.FindCollectionByNameOrId(CurrentPlaybacksCollection); err == nil {
		t.Fatal("base Ensure created current playbacks sidecar")
	}
}

func TestValidateCurrentPlaybacksExactAndWriteFree(t *testing.T) {
	app := testApp(t)
	if err := Ensure(app); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrentPlaybacks(app); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("missing sidecar Validate = %v, want unsupported", err)
	}

	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Save(CurrentPlaybacks(sessions.Id)); err != nil {
		t.Fatal(err)
	}
	before := fullFingerprint(t, app)
	if err := ValidateCurrentPlaybacks(app); err != nil {
		t.Fatalf("exact Validate: %v", err)
	}
	if after := fullFingerprint(t, app); after != before {
		t.Fatal("ValidateCurrentPlaybacks wrote the database")
	}

	// Row hole is allowed: no current playbacks for existing sessions.
	userID := saveTestUser(t, app, "alice-cp", "syn-alice-cp")
	saveTestSession(t, app, userID, "hash-cp-1")
	before = fullFingerprint(t, app)
	if err := ValidateCurrentPlaybacks(app); err != nil {
		t.Fatalf("Validate with row hole: %v", err)
	}
	if after := fullFingerprint(t, app); after != before {
		t.Fatal("ValidateCurrentPlaybacks wrote with row hole")
	}
}

func TestValidateCurrentPlaybacksRejectsDriftWithoutWrites(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*core.Collection)
	}{
		{"rule", func(c *core.Collection) { s := "id != ''"; c.ListRule = &s }},
		{"field_max", func(c *core.Collection) {
			c.Fields.GetByName("item_id").(*core.TextField).Max = 81
		}},
		{"cascade", func(c *core.Collection) {
			c.Fields.GetByName("gateway_session").(*core.RelationField).CascadeDelete = false
		}},
		{"index", func(c *core.Collection) {
			c.AddIndex("idx_gateway_current_playbacks_extra", false, "item_id", "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := testApp(t)
			if err := Ensure(app); err != nil {
				t.Fatal(err)
			}
			sessions, _ := app.FindCollectionByNameOrId("gateway_sessions")
			if err := app.Save(CurrentPlaybacks(sessions.Id)); err != nil {
				t.Fatal(err)
			}
			c, _ := app.FindCollectionByNameOrId(CurrentPlaybacksCollection)
			tc.mut(c)
			if err := app.Save(c); err != nil {
				t.Fatal(err)
			}
			before := fullFingerprint(t, app)
			if err := ValidateCurrentPlaybacks(app); !errors.Is(err, ErrUnsupportedSchema) {
				t.Fatalf("Validate = %v, want unsupported", err)
			}
			if after := fullFingerprint(t, app); after != before {
				t.Fatal("Validate repaired drifted sidecar")
			}
		})
	}
}

func TestValidateCurrentPlaybacksRejectsPhysicalDrift(t *testing.T) {
	app := testApp(t)
	if err := Ensure(app); err != nil {
		t.Fatal(err)
	}
	sessions, _ := app.FindCollectionByNameOrId("gateway_sessions")
	if err := app.Save(CurrentPlaybacks(sessions.Id)); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB().NewQuery("ALTER TABLE gateway_current_playbacks ADD COLUMN intruder TEXT").Execute(); err != nil {
		t.Fatal(err)
	}
	before := fullFingerprint(t, app)
	if err := ValidateCurrentPlaybacks(app); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("Validate = %v, want unsupported", err)
	}
	if after := fullFingerprint(t, app); after != before {
		t.Fatal("Validate repaired physical drift")
	}
}

func TestEnsureAcceptsExactCurrentPlaybacksSidecarWriteFree(t *testing.T) {
	app := testApp(t)
	if err := Ensure(app); err != nil {
		t.Fatal(err)
	}
	sessions, _ := app.FindCollectionByNameOrId("gateway_sessions")
	if err := app.Save(CurrentPlaybacks(sessions.Id)); err != nil {
		t.Fatal(err)
	}
	// Seed a current playback row so fingerprint includes data.
	userID := saveTestUser(t, app, "bob-cp", "syn-bob-cp")
	sessionID := saveTestSession(t, app, userID, "hash-bob-cp")
	saveTestCurrentPlayback(t, app, sessionID)

	before := fullFingerprint(t, app)
	if err := Ensure(app); err != nil {
		t.Fatalf("Ensure with exact sidecar: %v", err)
	}
	if after := fullFingerprint(t, app); after != before {
		t.Fatal("Ensure wrote when exact sidecar was present")
	}
}

func TestCurrentPlaybacksPersistedPhysicalValidation(t *testing.T) {
	app := testApp(t)
	if err := Ensure(app); err != nil {
		t.Fatal(err)
	}
	sessions, _ := app.FindCollectionByNameOrId("gateway_sessions")
	want := CurrentPlaybacks(sessions.Id)
	if err := app.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := app.FindCollectionByNameOrId(CurrentPlaybacksCollection)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild want with the same relation target; collectionEqual ignores id.
	want = CurrentPlaybacks(sessions.Id)
	if !collectionEqual(got, want) {
		t.Fatalf("persisted differs: %s", collectionDifference(got, want))
	}
	if err := validateCollectionRaw(app, got, want); err != nil {
		t.Fatalf("raw: %v", err)
	}
	if err := validateTable(app, got); err != nil {
		t.Fatalf("table: %v", err)
	}
}

func saveTestCurrentPlayback(t *testing.T, app core.App, sessionID string) string {
	t.Helper()
	col, err := app.FindCollectionByNameOrId(CurrentPlaybacksCollection)
	if err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(col)
	record.Set("gateway_session", sessionID)
	record.Set("item_id", "item-1")
	record.Set("item_snapshot_json", "{}")
	record.Set("play_state_json", "{}")
	record.Set("started_at", "2030-01-02 03:04:05.000Z")
	record.Set("last_reported_at", "2030-01-02 03:05:05.000Z")
	if err := app.Save(record); err != nil {
		t.Fatalf("save current playback: %v", err)
	}
	return record.Id
}
