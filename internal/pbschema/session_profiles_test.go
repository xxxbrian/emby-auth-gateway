package pbschema

import (
	"errors"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/sessionid"
)

func TestSessionProfilesBuilderExactShape(t *testing.T) {
	t.Parallel()

	const sessionID = "pbc_sessions_test01"
	c := SessionProfiles(sessionID)

	if c.Name != SessionProfilesCollection {
		t.Fatalf("name = %q", c.Name)
	}
	if c.Type != core.CollectionTypeBase {
		t.Fatalf("type = %q", c.Type)
	}
	if c.ListRule != nil || c.ViewRule != nil || c.CreateRule != nil || c.UpdateRule != nil || c.DeleteRule != nil {
		t.Fatalf("API rules must all be nil: list=%v view=%v create=%v update=%v delete=%v",
			c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule)
	}

	// id is auto; then the six declared fields.
	wantNames := []string{"id", "gateway_session", "public_session_id", "capabilities_json", "last_activity_at", "created", "updated"}
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

	pub := c.Fields.GetByName("public_session_id").(*core.TextField)
	if !pub.Required || pub.Min != sessionid.Length || pub.Max != sessionid.Length || pub.Pattern != sessionid.Pattern {
		t.Fatalf("public_session_id = %#v", pub)
	}

	caps := c.Fields.GetByName("capabilities_json").(*core.TextField)
	if !caps.Required || caps.Min != 2 || caps.Max != 262144 {
		t.Fatalf("capabilities_json = %#v", caps)
	}

	activity := c.Fields.GetByName("last_activity_at").(*core.DateField)
	if !activity.Required {
		t.Fatal("last_activity_at must be required")
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
		`CREATE UNIQUE INDEX idx_gateway_session_profiles_session ON gateway_session_profiles (gateway_session)`,
		`CREATE UNIQUE INDEX idx_gateway_session_profiles_public_id ON gateway_session_profiles (public_session_id)`,
	}
	// PocketBase AddIndex stores SQL; compare via indexesEqual normalization path.
	if !indexesEqual(c.Indexes, wantIndexes) {
		// Accept either quoted or unquoted forms by checking names present.
		gotNames := map[string]bool{}
		for _, idx := range c.Indexes {
			if strings.Contains(idx, "idx_gateway_session_profiles_session") {
				gotNames["session"] = true
			}
			if strings.Contains(idx, "idx_gateway_session_profiles_public_id") {
				gotNames["public"] = true
			}
			if !strings.Contains(strings.ToUpper(idx), "UNIQUE") {
				t.Fatalf("index is not unique: %s", idx)
			}
		}
		if len(c.Indexes) != 2 || !gotNames["session"] || !gotNames["public"] {
			t.Fatalf("indexes = %#v", c.Indexes)
		}
	}
}

func TestSessionProfilesNotInBaseRequiredOrEnsureCreateSet(t *testing.T) {
	t.Parallel()

	for _, name := range requiredNames {
		if name == SessionProfilesCollection {
			t.Fatal("SessionProfilesCollection must not be in requiredNames")
		}
	}
	if requiredCollectionName(SessionProfilesCollection) {
		t.Fatal("requiredCollectionName must not treat sidecar as base-required")
	}

	// Fresh Ensure must not create the sidecar.
	app := testApp(t)
	if err := Ensure(app); err != nil {
		t.Fatal(err)
	}
	if _, err := app.FindCollectionByNameOrId(SessionProfilesCollection); err == nil {
		t.Fatal("base Ensure created session profiles sidecar")
	}
}

func TestValidateSessionProfilesExactAndWriteFree(t *testing.T) {
	app := testApp(t)
	if err := Ensure(app); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSessionProfiles(app); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("missing sidecar Validate = %v, want unsupported", err)
	}

	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Save(SessionProfiles(sessions.Id)); err != nil {
		t.Fatal(err)
	}
	before := fullFingerprint(t, app)
	if err := ValidateSessionProfiles(app); err != nil {
		t.Fatalf("exact Validate: %v", err)
	}
	if after := fullFingerprint(t, app); after != before {
		t.Fatal("ValidateSessionProfiles wrote the database")
	}

	// Row hole is allowed: no profiles for existing sessions.
	userID := saveTestUser(t, app, "alice", "syn-alice")
	saveTestSession(t, app, userID, "hash-1")
	before = fullFingerprint(t, app)
	if err := ValidateSessionProfiles(app); err != nil {
		t.Fatalf("Validate with row hole: %v", err)
	}
	if after := fullFingerprint(t, app); after != before {
		t.Fatal("ValidateSessionProfiles wrote with row hole")
	}
}

func TestValidateSessionProfilesRejectsDriftWithoutWrites(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*core.Collection)
	}{
		{"rule", func(c *core.Collection) { s := "id != ''"; c.ListRule = &s }},
		{"field_max", func(c *core.Collection) {
			c.Fields.GetByName("public_session_id").(*core.TextField).Max = 41
		}},
		{"cascade", func(c *core.Collection) {
			c.Fields.GetByName("gateway_session").(*core.RelationField).CascadeDelete = false
		}},
		{"index", func(c *core.Collection) {
			c.AddIndex("idx_gateway_session_profiles_extra", false, "capabilities_json", "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := testApp(t)
			if err := Ensure(app); err != nil {
				t.Fatal(err)
			}
			sessions, _ := app.FindCollectionByNameOrId("gateway_sessions")
			if err := app.Save(SessionProfiles(sessions.Id)); err != nil {
				t.Fatal(err)
			}
			c, _ := app.FindCollectionByNameOrId(SessionProfilesCollection)
			tc.mut(c)
			if err := app.Save(c); err != nil {
				t.Fatal(err)
			}
			before := fullFingerprint(t, app)
			if err := ValidateSessionProfiles(app); !errors.Is(err, ErrUnsupportedSchema) {
				t.Fatalf("Validate = %v, want unsupported", err)
			}
			if after := fullFingerprint(t, app); after != before {
				t.Fatal("Validate repaired drifted sidecar")
			}
		})
	}
}

func TestValidateSessionProfilesRejectsPhysicalDrift(t *testing.T) {
	app := testApp(t)
	if err := Ensure(app); err != nil {
		t.Fatal(err)
	}
	sessions, _ := app.FindCollectionByNameOrId("gateway_sessions")
	if err := app.Save(SessionProfiles(sessions.Id)); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB().NewQuery("ALTER TABLE gateway_session_profiles ADD COLUMN intruder TEXT").Execute(); err != nil {
		t.Fatal(err)
	}
	before := fullFingerprint(t, app)
	if err := ValidateSessionProfiles(app); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("Validate = %v, want unsupported", err)
	}
	if after := fullFingerprint(t, app); after != before {
		t.Fatal("Validate repaired physical drift")
	}
}

func TestEnsureAcceptsExactSessionProfilesSidecarWriteFree(t *testing.T) {
	app := testApp(t)
	if err := Ensure(app); err != nil {
		t.Fatal(err)
	}
	sessions, _ := app.FindCollectionByNameOrId("gateway_sessions")
	if err := app.Save(SessionProfiles(sessions.Id)); err != nil {
		t.Fatal(err)
	}
	// Seed a profile row so fingerprint includes data.
	userID := saveTestUser(t, app, "bob", "syn-bob")
	sessionID := saveTestSession(t, app, userID, "hash-bob")
	saveTestProfile(t, app, sessionID)

	before := fullFingerprint(t, app)
	if err := Ensure(app); err != nil {
		t.Fatalf("Ensure with exact sidecar: %v", err)
	}
	if after := fullFingerprint(t, app); after != before {
		t.Fatal("Ensure wrote when exact sidecar was present")
	}
}

func TestSessionProfilesPersistedPhysicalValidation(t *testing.T) {
	app := testApp(t)
	if err := Ensure(app); err != nil {
		t.Fatal(err)
	}
	sessions, _ := app.FindCollectionByNameOrId("gateway_sessions")
	want := SessionProfiles(sessions.Id)
	if err := app.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := app.FindCollectionByNameOrId(SessionProfilesCollection)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild want with the same relation target; collectionEqual ignores id.
	want = SessionProfiles(sessions.Id)
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

func saveTestUser(t *testing.T, app core.App, username, synthetic string) string {
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

func saveTestSession(t *testing.T, app core.App, userID, tokenHash string) string {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(col)
	record.Set("gateway_token_hash", tokenHash)
	record.Set("gateway_user", userID)
	record.Set("synthetic_user_id", "syn")
	record.Set("expires_at", "2030-01-02 03:04:05.000Z")
	if err := app.Save(record); err != nil {
		t.Fatalf("save session: %v", err)
	}
	return record.Id
}

func saveTestProfile(t *testing.T, app core.App, sessionID string) string {
	t.Helper()
	col, err := app.FindCollectionByNameOrId(SessionProfilesCollection)
	if err != nil {
		t.Fatal(err)
	}
	id, err := sessionid.New()
	if err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(col)
	record.Set("gateway_session", sessionID)
	record.Set("public_session_id", id)
	record.Set("capabilities_json", "{}")
	record.Set("last_activity_at", "2030-01-02 03:04:05.000Z")
	if err := app.Save(record); err != nil {
		t.Fatalf("save profile: %v", err)
	}
	return record.Id
}
