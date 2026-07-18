package pbmigrations

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
	"github.com/xxxbrian/emby-auth-gateway/internal/sessionid"
)

// newBareApp bootstraps a PocketBase app with system migrations only.
// Unlike tests.NewTestAppWithConfig, it does NOT call RunAllMigrations, so
// global AppMigrations (including this package's production migration) are not
// auto-applied. That keeps private-list migration tests deterministic.
func newBareApp(t *testing.T) core.App {
	t.Helper()
	app := core.NewBaseApp(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "test",
	})
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(func() {
		_ = app.ResetBootstrapState()
	})
	return app
}

func TestSessionProfilesMigrationCreateBackfillHistoryIdempotent(t *testing.T) {
	app := newBareApp(t)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatal(err)
	}
	userID := saveUser(t, app, "alice", "syn-alice")
	saveSession(t, app, userID, "hash-a")
	saveSession(t, app, userID, "hash-b")

	if err := applySessionProfilesMigration(app); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	assertMigrationHistory(t, app, migrationGatewaySessionProfiles, true)
	assertFullCoverage(t, app, 2)

	beforeIDs := profilePublicIDs(t, app)
	if err := applySessionProfilesMigration(app); err != nil {
		t.Fatalf("second migrate (history no-op): %v", err)
	}
	afterIDs := profilePublicIDs(t, app)
	if len(afterIDs) != len(beforeIDs) {
		t.Fatalf("profile count changed: %d -> %d", len(beforeIDs), len(afterIDs))
	}
	for k, v := range beforeIDs {
		if afterIDs[k] != v {
			t.Fatalf("public id for session %q changed: %q -> %q", k, v, afterIDs[k])
		}
	}
	assertFullCoverage(t, app, 2)
	assertMigrationHistory(t, app, migrationGatewaySessionProfiles, true)
}

func TestSessionProfilesMigrationFreshBase(t *testing.T) {
	app := newBareApp(t)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatal(err)
	}
	if _, err := app.FindCollectionByNameOrId(pbschema.SessionProfilesCollection); err == nil {
		t.Fatal("sidecar must not exist before migration on fresh base")
	}
	if err := applySessionProfilesMigration(app); err != nil {
		t.Fatalf("migrate fresh: %v", err)
	}
	if err := pbschema.ValidateSessionProfiles(app); err != nil {
		t.Fatal(err)
	}
	assertFullCoverage(t, app, 0)
	assertMigrationHistory(t, app, migrationGatewaySessionProfiles, true)
}

func TestSessionProfilesMigrationExistingExactSidecar(t *testing.T) {
	app := newBareApp(t)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatal(err)
	}
	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Save(pbschema.SessionProfiles(sessions.Id)); err != nil {
		t.Fatal(err)
	}
	userID := saveUser(t, app, "bob", "syn-bob")
	sessionID := saveSession(t, app, userID, "hash-bob")
	saveProfile(t, app, sessionID, mustSessionID(t), "{}")
	hole := saveSession(t, app, userID, "hash-hole")

	if err := applySessionProfilesMigration(app); err != nil {
		t.Fatalf("migrate with existing sidecar: %v", err)
	}
	assertFullCoverage(t, app, 2)
	profiles, err := app.FindAllRecords(pbschema.SessionProfilesCollection)
	if err != nil {
		t.Fatal(err)
	}
	foundHole := false
	for _, p := range profiles {
		if p.GetString("gateway_session") == hole {
			foundHole = true
			if p.GetString("capabilities_json") != "{}" {
				t.Fatalf("backfill caps = %q", p.GetString("capabilities_json"))
			}
			if !sessionid.Valid(p.GetString("public_session_id")) {
				t.Fatalf("backfill public id invalid: %q", p.GetString("public_session_id"))
			}
		}
	}
	if !foundHole {
		t.Fatal("hole session was not backfilled")
	}
}

func TestSessionProfilesMigrationRejectsDriftedSidecar(t *testing.T) {
	app := newBareApp(t)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatal(err)
	}
	sessions, _ := app.FindCollectionByNameOrId("gateway_sessions")
	if err := app.Save(pbschema.SessionProfiles(sessions.Id)); err != nil {
		t.Fatal(err)
	}
	c, _ := app.FindCollectionByNameOrId(pbschema.SessionProfilesCollection)
	c.Fields.GetByName("public_session_id").(*core.TextField).Max = 41
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}
	err := applySessionProfilesMigration(app)
	if err == nil {
		t.Fatal("expected drifted sidecar rejection")
	}
	assertMigrationHistory(t, app, migrationGatewaySessionProfiles, false)
}

func TestSessionProfilesMigrationRejectsOrphan(t *testing.T) {
	app := newBareApp(t)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatal(err)
	}
	sessions, _ := app.FindCollectionByNameOrId("gateway_sessions")
	if err := app.Save(pbschema.SessionProfiles(sessions.Id)); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB().NewQuery(`
		INSERT INTO gateway_session_profiles (id, gateway_session, public_session_id, capabilities_json, last_activity_at, created, updated)
		VALUES ('orphanprofile01', 'missing_session_id', {:pid}, '{}', '2030-01-01 00:00:00.000Z', '2030-01-01 00:00:00.000Z', '2030-01-01 00:00:00.000Z')
	`).Bind(map[string]any{"pid": mustSessionID(t)}).Execute(); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}
	err := applySessionProfilesMigration(app)
	if err == nil || !strings.Contains(err.Error(), "orphan") {
		t.Fatalf("error = %v, want orphan", err)
	}
	assertMigrationHistory(t, app, migrationGatewaySessionProfiles, false)
}

func TestSessionProfilesMigrationRejectsMalformedPublicID(t *testing.T) {
	app := newBareApp(t)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatal(err)
	}
	sessions, _ := app.FindCollectionByNameOrId("gateway_sessions")
	if err := app.Save(pbschema.SessionProfiles(sessions.Id)); err != nil {
		t.Fatal(err)
	}
	userID := saveUser(t, app, "bad", "syn-bad")
	sessionID := saveSession(t, app, userID, "hash-bad")
	if _, err := app.DB().NewQuery(`
		INSERT INTO gateway_session_profiles (id, gateway_session, public_session_id, capabilities_json, last_activity_at, created, updated)
		VALUES ('badprofile00001', {:sid}, 'not-a-session-id', '{}', '2030-01-01 00:00:00.000Z', '2030-01-01 00:00:00.000Z', '2030-01-01 00:00:00.000Z')
	`).Bind(map[string]any{"sid": sessionID}).Execute(); err != nil {
		t.Fatal(err)
	}
	err := applySessionProfilesMigration(app)
	if err == nil {
		t.Fatal("expected malformed public id rejection")
	}
	if !strings.Contains(err.Error(), "public_session_id") && !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("error = %v", err)
	}
	assertMigrationHistory(t, app, migrationGatewaySessionProfiles, false)
}

func TestSessionProfilesMigrationRejectsMalformedCapabilities(t *testing.T) {
	app := newBareApp(t)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatal(err)
	}
	sessions, _ := app.FindCollectionByNameOrId("gateway_sessions")
	if err := app.Save(pbschema.SessionProfiles(sessions.Id)); err != nil {
		t.Fatal(err)
	}
	userID := saveUser(t, app, "cap", "syn-cap")
	sessionID := saveSession(t, app, userID, "hash-cap")
	if _, err := app.DB().NewQuery(`
		INSERT INTO gateway_session_profiles (id, gateway_session, public_session_id, capabilities_json, last_activity_at, created, updated)
		VALUES ('badcaps00000001', {:sid}, {:pid}, 'x', '2030-01-01 00:00:00.000Z', '2030-01-01 00:00:00.000Z', '2030-01-01 00:00:00.000Z')
	`).Bind(map[string]any{"sid": sessionID, "pid": mustSessionID(t)}).Execute(); err != nil {
		t.Fatal(err)
	}
	err := applySessionProfilesMigration(app)
	if err == nil {
		t.Fatal("expected malformed capabilities rejection")
	}
	if !strings.Contains(err.Error(), "capabilities_json") && !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("error = %v", err)
	}
	assertMigrationHistory(t, app, migrationGatewaySessionProfiles, false)
}

func TestSessionProfilesMigrationRejectsNonObjectCapabilitiesJSON(t *testing.T) {
	// Length-valid documents that are not JSON objects must fail migration
	// atomically (no history write), not only the short "x" length case.
	cases := []struct {
		name string
		caps string
	}{
		{name: "length_valid_malformed", caps: `{"PlayableMediaTypes":`},
		{name: "top_level_array", caps: `[]`},
		{name: "top_level_null", caps: `null`},
		{name: "top_level_string", caps: `"not-an-object"`},
		{name: "top_level_number", caps: `42`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := newBareApp(t)
			if err := pbschema.Ensure(app); err != nil {
				t.Fatal(err)
			}
			sessions, _ := app.FindCollectionByNameOrId("gateway_sessions")
			if err := app.Save(pbschema.SessionProfiles(sessions.Id)); err != nil {
				t.Fatal(err)
			}
			userID := saveUser(t, app, "cap-"+tc.name, "syn-cap-"+tc.name)
			sessionID := saveSession(t, app, userID, "hash-cap-"+tc.name)
			if _, err := app.DB().NewQuery(`
				INSERT INTO gateway_session_profiles (id, gateway_session, public_session_id, capabilities_json, last_activity_at, created, updated)
				VALUES ({:id}, {:sid}, {:pid}, {:caps}, '2030-01-01 00:00:00.000Z', '2030-01-01 00:00:00.000Z', '2030-01-01 00:00:00.000Z')
			`).Bind(map[string]any{
				"id":   fmt.Sprintf("badcap%09d", len(tc.name)),
				"sid":  sessionID,
				"pid":  mustSessionID(t),
				"caps": tc.caps,
			}).Execute(); err != nil {
				t.Fatal(err)
			}
			// Also seed a second session without a profile to prove backfill does
			// not partially succeed when validation fails.
			saveSession(t, app, userID, "hash-hole-"+tc.name)

			err := applySessionProfilesMigration(app)
			if err == nil {
				t.Fatal("expected capabilities_json rejection")
			}
			msg := err.Error()
			if !strings.Contains(msg, "capabilities_json") && !strings.Contains(msg, "malformed") {
				t.Fatalf("error = %v, want capabilities_json integrity failure", err)
			}
			assertMigrationHistory(t, app, migrationGatewaySessionProfiles, false)

			// Atomic failure: no hole backfill rows written for the second session.
			profiles, listErr := app.FindAllRecords(pbschema.SessionProfilesCollection)
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(profiles) != 1 {
				t.Fatalf("profiles after failed migrate = %d, want only the pre-seeded corrupt row", len(profiles))
			}
		})
	}
}

func TestValidateCapabilitiesJSON(t *testing.T) {
	t.Parallel()
	if err := validateCapabilitiesJSON(`{}`); err != nil {
		t.Fatalf("empty object: %v", err)
	}
	if err := validateCapabilitiesJSON(`{"PlayableMediaTypes":["Video"]}`); err != nil {
		t.Fatalf("object: %v", err)
	}
	if err := validateCapabilitiesJSON(`{"Custom":true,"Huge":9007199254740993}`); err != nil {
		t.Fatalf("unknown fields: %v", err)
	}
	if err := validateCapabilitiesJSON(`{`); err == nil {
		t.Fatal("malformed: want error")
	}
	if err := validateCapabilitiesJSON(`null`); err == nil {
		t.Fatal("null: want error")
	}
	if err := validateCapabilitiesJSON(`[]`); err == nil {
		t.Fatal("array: want error")
	}
	if err := validateCapabilitiesJSON(`x`); err == nil {
		t.Fatal("short non-json: want error")
	}
	if err := validateCapabilitiesJSON(strings.Repeat("a", sessionCapabilitiesJSONMaxBytes+1)); err == nil {
		t.Fatal("oversize: want error")
	}
	// Runtime-divergent known field shapes must fail the shared validator.
	if err := validateCapabilitiesJSON(`{"SupportsSync":"yes"}`); err == nil {
		t.Fatal("wrong bool type: want error")
	}
	if err := validateCapabilitiesJSON(`{"PlayableMediaTypes":"Video"}`); err == nil {
		t.Fatal("array wrong type: want error")
	}
	if err := validateCapabilitiesJSON(`{"PlayableMediaTypes":[1]}`); err == nil {
		t.Fatal("non-string array item: want error")
	}
	if err := validateCapabilitiesJSON(`{"DeviceProfile":[]}`); err == nil {
		t.Fatal("DeviceProfile array: want error")
	}
}

func TestSessionProfilesMigrationRejectsRuntimeDivergentCapabilities(t *testing.T) {
	// Values that previously passed weak migration shape checks but fail runtime
	// ParseSessionCapabilities / sessioncaps.Validate must fail migration atomically.
	cases := []struct {
		name string
		caps string
	}{
		{name: "wrong_bool_type", caps: `{"SupportsMediaControl":"true"}`},
		{name: "array_wrong_type", caps: `{"SupportedCommands":"Play"}`},
		{name: "non_string_array_item", caps: `{"PlayableMediaTypes":[1,2]}`},
		{name: "device_profile_array", caps: `{"DeviceProfile":[]}`},
		{name: "oversized_array_entry", caps: `{"PlayableMediaTypes":["` + strings.Repeat("z", 65) + `"]}`},
		{name: "too_many_media_types", caps: tooManyMediaTypesJSON()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := newBareApp(t)
			if err := pbschema.Ensure(app); err != nil {
				t.Fatal(err)
			}
			sessions, _ := app.FindCollectionByNameOrId("gateway_sessions")
			if err := app.Save(pbschema.SessionProfiles(sessions.Id)); err != nil {
				t.Fatal(err)
			}
			userID := saveUser(t, app, "rt-"+tc.name, "syn-rt-"+tc.name)
			sessionID := saveSession(t, app, userID, "hash-rt-"+tc.name)
			if _, err := app.DB().NewQuery(`
				INSERT INTO gateway_session_profiles (id, gateway_session, public_session_id, capabilities_json, last_activity_at, created, updated)
				VALUES ({:id}, {:sid}, {:pid}, {:caps}, '2030-01-01 00:00:00.000Z', '2030-01-01 00:00:00.000Z', '2030-01-01 00:00:00.000Z')
			`).Bind(map[string]any{
				"id":   fmt.Sprintf("rtdiv%09d", len(tc.name)),
				"sid":  sessionID,
				"pid":  mustSessionID(t),
				"caps": tc.caps,
			}).Execute(); err != nil {
				t.Fatal(err)
			}
			// Second session without profile: must not be backfilled on failure.
			saveSession(t, app, userID, "hash-rt-hole-"+tc.name)

			err := applySessionProfilesMigration(app)
			if err == nil {
				t.Fatal("expected capabilities rejection")
			}
			if !strings.Contains(err.Error(), "capabilities_json") {
				t.Fatalf("error = %v, want capabilities_json", err)
			}
			assertMigrationHistory(t, app, migrationGatewaySessionProfiles, false)

			profiles, listErr := app.FindAllRecords(pbschema.SessionProfilesCollection)
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(profiles) != 1 {
				t.Fatalf("profiles after failed migrate = %d, want only pre-seeded corrupt row", len(profiles))
			}
		})
	}

	// Accepted valid unknown fields still migrate.
	t.Run("accepts_unknown_fields", func(t *testing.T) {
		app := newBareApp(t)
		if err := pbschema.Ensure(app); err != nil {
			t.Fatal(err)
		}
		sessions, _ := app.FindCollectionByNameOrId("gateway_sessions")
		if err := app.Save(pbschema.SessionProfiles(sessions.Id)); err != nil {
			t.Fatal(err)
		}
		userID := saveUser(t, app, "rt-ok", "syn-rt-ok")
		sessionID := saveSession(t, app, userID, "hash-rt-ok")
		if _, err := app.DB().NewQuery(`
			INSERT INTO gateway_session_profiles (id, gateway_session, public_session_id, capabilities_json, last_activity_at, created, updated)
			VALUES ('rtok000000001', {:sid}, {:pid}, {:caps}, '2030-01-01 00:00:00.000Z', '2030-01-01 00:00:00.000Z', '2030-01-01 00:00:00.000Z')
		`).Bind(map[string]any{
			"sid":  sessionID,
			"pid":  mustSessionID(t),
			"caps": `{"CustomFlag":true,"Huge":9007199254740993,"DeviceProfile":{"Name":"x"}}`,
		}).Execute(); err != nil {
			t.Fatal(err)
		}
		if err := applySessionProfilesMigration(app); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		assertMigrationHistory(t, app, migrationGatewaySessionProfiles, true)
	})
}

func tooManyMediaTypesJSON() string {
	var b strings.Builder
	b.WriteString(`{"PlayableMediaTypes":[`)
	for i := 0; i < 33; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"M%d"`, i)
	}
	b.WriteString(`]}`)
	return b.String()
}

func TestSessionProfilesMigrationRejectsDuplicatePublicAndRelation(t *testing.T) {
	app := newBareApp(t)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatal(err)
	}
	sessions, _ := app.FindCollectionByNameOrId("gateway_sessions")
	if err := app.Save(pbschema.SessionProfiles(sessions.Id)); err != nil {
		t.Fatal(err)
	}
	userID := saveUser(t, app, "rel", "syn-rel")
	sessionID := saveSession(t, app, userID, "hash-rel")
	if _, err := app.DB().NewQuery(`DROP INDEX IF EXISTS idx_gateway_session_profiles_session`).Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB().NewQuery(`DROP INDEX IF EXISTS idx_gateway_session_profiles_public_id`).Execute(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := app.DB().NewQuery(`
			INSERT INTO gateway_session_profiles (id, gateway_session, public_session_id, capabilities_json, last_activity_at, created, updated)
			VALUES ({:id}, {:sid}, {:pid}, '{}', '2030-01-01 00:00:00.000Z', '2030-01-01 00:00:00.000Z', '2030-01-01 00:00:00.000Z')
		`).Bind(map[string]any{
			"id":  fmt.Sprintf("duprel%010d", i),
			"sid": sessionID,
			"pid": mustSessionID(t),
		}).Execute(); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	err := rejectInvalidSessionProfiles(app)
	if err == nil || !strings.Contains(err.Error(), "duplicate gateway_session") {
		t.Fatalf("duplicate relation err = %v", err)
	}

	app2 := newBareApp(t)
	if err := pbschema.Ensure(app2); err != nil {
		t.Fatal(err)
	}
	sessions2, _ := app2.FindCollectionByNameOrId("gateway_sessions")
	if err := app2.Save(pbschema.SessionProfiles(sessions2.Id)); err != nil {
		t.Fatal(err)
	}
	user2 := saveUser(t, app2, "pub", "syn-pub")
	sA := saveSession(t, app2, user2, "hash-a")
	sB := saveSession(t, app2, user2, "hash-b")
	if _, err := app2.DB().NewQuery(`DROP INDEX IF EXISTS idx_gateway_session_profiles_public_id`).Execute(); err != nil {
		t.Fatal(err)
	}
	shared := mustSessionID(t)
	for i, sid := range []string{sA, sB} {
		if _, err := app2.DB().NewQuery(`
			INSERT INTO gateway_session_profiles (id, gateway_session, public_session_id, capabilities_json, last_activity_at, created, updated)
			VALUES ({:id}, {:sid}, {:pid}, '{}', '2030-01-01 00:00:00.000Z', '2030-01-01 00:00:00.000Z', '2030-01-01 00:00:00.000Z')
		`).Bind(map[string]any{
			"id":  fmt.Sprintf("duppub%010d", i),
			"sid": sid,
			"pid": shared,
		}).Execute(); err != nil {
			t.Fatalf("insert pub %d: %v", i, err)
		}
	}
	err = rejectInvalidSessionProfiles(app2)
	if err == nil || !strings.Contains(err.Error(), "duplicate public_session_id") {
		t.Fatalf("duplicate public err = %v", err)
	}
}

func TestValidateExtensionsShapeOnlyAllowsRowHole(t *testing.T) {
	app := newBareApp(t)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatal(err)
	}
	if err := validateExtensions(app); err == nil {
		t.Fatal("expected missing sidecar failure")
	}
	sessions, _ := app.FindCollectionByNameOrId("gateway_sessions")
	if err := app.Save(pbschema.SessionProfiles(sessions.Id)); err != nil {
		t.Fatal(err)
	}
	userID := saveUser(t, app, "hole", "syn-hole")
	saveSession(t, app, userID, "hash-hole")
	before := countProfiles(t, app)
	if err := validateExtensions(app); err != nil {
		t.Fatalf("validateExtensions with hole: %v", err)
	}
	if countProfiles(t, app) != before {
		t.Fatal("validateExtensions wrote profile rows")
	}
}

func TestSessionProfilesMigrationActivityFromSessionCreated(t *testing.T) {
	app := newBareApp(t)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatal(err)
	}
	userID := saveUser(t, app, "act", "syn-act")
	sessionID := saveSession(t, app, userID, "hash-act")
	session, err := app.FindRecordById("gateway_sessions", sessionID)
	if err != nil {
		t.Fatal(err)
	}
	created := session.GetDateTime("created")
	if created.IsZero() {
		t.Fatal("session created is zero")
	}
	if err := applySessionProfilesMigration(app); err != nil {
		t.Fatal(err)
	}
	profiles, err := app.FindAllRecords(pbschema.SessionProfilesCollection)
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profiles = %d err=%v", len(profiles), err)
	}
	got := profiles[0].GetDateTime("last_activity_at")
	if !got.Time().Equal(created.Time()) {
		t.Fatalf("last_activity_at = %v, want session created %v", got, created)
	}
}

func TestSessionProfilesDownUnsupported(t *testing.T) {
	err := downGatewaySessionProfiles(nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("down = %v", err)
	}
}

func TestSessionProfilesMigrationSortedBackfillOrder(t *testing.T) {
	app := newBareApp(t)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatal(err)
	}
	userID := saveUser(t, app, "ord", "syn-ord")
	for i := 0; i < 5; i++ {
		saveSession(t, app, userID, fmt.Sprintf("hash-ord-%d", i))
	}
	var order []string
	hook := app.OnRecordCreate().BindFunc(func(e *core.RecordEvent) error {
		if e.Record.Collection().Name == pbschema.SessionProfilesCollection {
			order = append(order, e.Record.GetString("gateway_session"))
		}
		return e.Next()
	})
	defer app.OnRecordCreate().Unbind(hook)

	if err := applySessionProfilesMigration(app); err != nil {
		t.Fatal(err)
	}
	if len(order) != 5 {
		t.Fatalf("backfill order len = %d (%#v)", len(order), order)
	}
	for i := 1; i < len(order); i++ {
		if order[i-1] > order[i] {
			t.Fatalf("backfill not sorted by session id: %#v", order)
		}
	}
}

func TestApplyFinalValidatorRequiresSidecar(t *testing.T) {
	app := newBareApp(t)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatal(err)
	}
	list := core.MigrationsList{}
	list.Register(func(core.App) error { return nil }, nil, "1_noop.go")
	err := applyPrivate(app, list, validateExtensions)
	if err == nil {
		t.Fatal("expected final validator to fail without sidecar")
	}
}

func TestProductionApplyCreatesSidecar(t *testing.T) {
	// Production Apply uses global AppMigrations (registered in init).
	app := newBareApp(t)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatal(err)
	}
	userID := saveUser(t, app, "prod", "syn-prod")
	saveSession(t, app, userID, "hash-prod")
	if err := Apply(app); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertMigrationHistory(t, app, migrationGatewaySessionProfiles, true)
	assertFullCoverage(t, app, 1)
	// Second Apply is history no-op + write-free validator.
	if err := Apply(app); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
}

func TestIsRecognizedUniqueCollision(t *testing.T) {
	t.Parallel()
	if isRecognizedUniqueCollision(nil) {
		t.Fatal("nil")
	}
	if !isRecognizedUniqueCollision(errors.New("UNIQUE constraint failed: gateway_session_profiles.public_session_id")) {
		t.Fatal("sqlite unique")
	}
	if !isRecognizedUniqueCollision(errors.New("public_session_id: Value must be unique.")) {
		t.Fatal("validation message")
	}
	if !isRecognizedUniqueCollision(errors.New("validation_not_unique")) {
		t.Fatal("code")
	}
	if isRecognizedUniqueCollision(errors.New("connection reset")) {
		t.Fatal("unrelated")
	}
}

// applySessionProfilesMigration runs the production up function via a private
// MigrationsList so tests never reset global core.AppMigrations.
func applySessionProfilesMigration(app core.App) error {
	list := core.MigrationsList{}
	list.Register(upGatewaySessionProfiles, downGatewaySessionProfiles, migrationGatewaySessionProfiles)
	return applyPrivate(app, list, validateExtensions)
}

func saveUser(t *testing.T, app core.App, username, synthetic string) string {
	t.Helper()
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(users)
	record.Set("username", username)
	record.Set("email", username+"@example.test")
	record.Set("synthetic_user_id", synthetic)
	record.Set("enabled", true)
	record.SetPassword("test-pass-123")
	if err := app.Save(record); err != nil {
		t.Fatalf("save user: %v", err)
	}
	return record.Id
}

func saveSession(t *testing.T, app core.App, userID, tokenHash string) string {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(col)
	record.Set("gateway_token_hash", tokenHash)
	record.Set("gateway_user", userID)
	record.Set("synthetic_user_id", "syn")
	record.Set("expires_at", time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC))
	if err := app.Save(record); err != nil {
		t.Fatalf("save session: %v", err)
	}
	return record.Id
}

func saveProfile(t *testing.T, app core.App, sessionID, publicID, caps string) string {
	t.Helper()
	col, err := app.FindCollectionByNameOrId(pbschema.SessionProfilesCollection)
	if err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(col)
	record.Set("gateway_session", sessionID)
	record.Set("public_session_id", publicID)
	record.Set("capabilities_json", caps)
	record.Set("last_activity_at", time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC))
	if err := app.Save(record); err != nil {
		t.Fatalf("save profile: %v", err)
	}
	return record.Id
}

func mustSessionID(t *testing.T) string {
	t.Helper()
	id, err := sessionid.New()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func assertFullCoverage(t *testing.T, app core.App, wantSessions int) {
	t.Helper()
	if err := requireFullSessionProfileCoverage(app); err != nil {
		t.Fatalf("coverage: %v", err)
	}
	sessions, err := app.FindAllRecords("gateway_sessions")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != wantSessions {
		t.Fatalf("sessions = %d, want %d", len(sessions), wantSessions)
	}
	profiles, err := app.FindAllRecords(pbschema.SessionProfilesCollection)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != wantSessions {
		t.Fatalf("profiles = %d, want %d", len(profiles), wantSessions)
	}
	for _, p := range profiles {
		if !sessionid.Valid(p.GetString("public_session_id")) {
			t.Fatalf("invalid public id %q", p.GetString("public_session_id"))
		}
		if p.GetString("capabilities_json") != "{}" {
			t.Fatalf("caps = %q", p.GetString("capabilities_json"))
		}
	}
}

func profilePublicIDs(t *testing.T, app core.App) map[string]string {
	t.Helper()
	profiles, err := app.FindAllRecords(pbschema.SessionProfilesCollection)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(profiles))
	for _, p := range profiles {
		out[p.GetString("gateway_session")] = p.GetString("public_session_id")
	}
	return out
}

func countProfiles(t *testing.T, app core.App) int {
	t.Helper()
	profiles, err := app.FindAllRecords(pbschema.SessionProfilesCollection)
	if err != nil {
		t.Fatal(err)
	}
	return len(profiles)
}
