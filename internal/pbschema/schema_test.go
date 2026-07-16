package pbschema

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func TestEnsureFreshThenExisting(t *testing.T) {
	app := testApp(t)
	if err := Ensure(app); err != nil {
		t.Fatalf("Ensure fresh: %v", err)
	}
	for _, name := range append([]string{"users"}, requiredNames...) {
		if _, err := app.FindCollectionByNameOrId(name); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	ids := map[string]string{}
	for _, name := range requiredNames {
		c, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = c.Id
	}
	endpoint, err := app.FindCollectionByNameOrId("upstream_endpoints")
	if err != nil {
		t.Fatal(err)
	}
	if got := endpoint.Fields.GetByName("source").(*core.RelationField).CollectionId; got != ids["upstream_sources"] {
		t.Fatalf("endpoint source id = %q, want %q", got, ids["upstream_sources"])
	}
	if err := Ensure(app); err != nil {
		t.Fatalf("Ensure existing: %v", err)
	}
	for name, id := range ids {
		c, err := app.FindCollectionByNameOrId(name)
		if err != nil || c.Id != id {
			t.Fatalf("collection %s id changed", name)
		}
	}
}

func TestEnsureRejectsModifiedPristineUsers(t *testing.T) {
	app := testApp(t)
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	users.Fields.Add(&core.TextField{Name: "unexpected"})
	if err := app.Save(users); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(app); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("Ensure error = %v, want unsupported schema", err)
	}
	if _, err := app.FindCollectionByNameOrId("gateway_sessions"); err == nil {
		t.Fatal("Ensure wrote a rejected database")
	}
}

func TestEnsureRejectsPartialSchema(t *testing.T) {
	app := testApp(t)
	if err := app.Save(base("gateway_sessions")); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(app); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("Ensure error = %v, want unsupported schema", err)
	}
}

func TestA_GeneratedIDsAndRelations(t *testing.T) {
	first, second := testApp(t), testApp(t)
	for i, app := range []core.App{first, second} {
		// These were formerly hard-coded application IDs. Their occupation must
		// not affect a fresh schema because PocketBase owns new IDs.
		for n := range requiredNames {
			occupied := core.NewBaseCollection(fmt.Sprintf("occupied_%d_%d", i, n), fmt.Sprintf("pbcapp%010d", n+1))
			lock(occupied)
			if err := app.Save(occupied); err != nil {
				t.Fatal(err)
			}
		}
		if err := Ensure(app); err != nil {
			t.Fatalf("Ensure app %d: %v", i, err)
		}
	}
	for name, id := range appIDs(t, first) {
		if name != "users" && (!validApplicationID(id) || strings.HasPrefix(id, "pbcapp")) {
			t.Fatalf("%s has invalid or legacy literal id %q", name, id)
		}
	}
	before := schemaFingerprint(t, first)
	if err := Ensure(first); err != nil {
		t.Fatal(err)
	}
	if after := schemaFingerprint(t, first); after != before {
		t.Fatal("existing Ensure changed IDs, relations, or physical schema")
	}
	for _, tc := range []struct{ collection, field, target string }{
		{"gateway_sessions", "gateway_user", "upstream_sources"},
		{"audit_logs", "gateway_user", "upstream_sources"},
		{"upstream_endpoints", "source", "users"},
	} {
		t.Run(tc.collection+"_"+tc.field, func(t *testing.T) {
			app := testApp(t)
			if err := Ensure(app); err != nil {
				t.Fatal(err)
			}
			c, _ := app.FindCollectionByNameOrId(tc.collection)
			c.Fields.GetByName(tc.field).(*core.RelationField).CollectionId = appIDs(t, app)[tc.target]
			// PocketBase rejects changing a relation target through Save. Exercise
			// the validator with the malformed metadata while retaining the DB
			// fingerprint to prove validation itself performs no repair.
			snapshot, err := loadCollections(app)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.byName[tc.collection], snapshot.byID[c.Id] = c, c
			before := schemaFingerprint(t, app)
			if err := validateSnapshot(app, snapshot); !errors.Is(err, ErrUnsupportedSchema) {
				t.Fatalf("validate = %v", err)
			}
			if after := schemaFingerprint(t, app); after != before {
				t.Fatal("Ensure repaired invalid relation")
			}
		})
	}
}

func TestA_ApplicationIDValidator(t *testing.T) {
	for _, id := range []string{"", "bad-id", strings.Repeat("a", 101)} {
		if validApplicationID(id) {
			t.Fatalf("accepted invalid id %q", id)
		}
	}
	if !validApplicationID("opaque_ID_123") {
		t.Fatal("rejected valid opaque id")
	}
	users := configuredUsers()
	snapshot := &collectionSnapshot{byName: map[string]*core.Collection{"users": users}, byID: map[string]*core.Collection{users.Id: users}}
	for _, name := range requiredNames {
		c := core.NewBaseCollection(name, "valid_"+name)
		lock(c)
		snapshot.byName[name], snapshot.byID[c.Id] = c, c
	}
	snapshot.byName["audit_logs"].Id = snapshot.byName["gateway_sessions"].Id
	if _, err := canonicalIDs(snapshot, users); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("duplicate id = %v", err)
	}
}

func TestF_NoncanonicalApplicationIDsAreAccepted(t *testing.T) {
	app := testApp(t)
	users, _ := app.FindCollectionByNameOrId("users")
	configureUsers(users)
	if err := app.Save(users); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{"users": users.Id}
	for i, name := range requiredNames {
		ids[name] = fmt.Sprintf("opaque_%02d_schema", i)
	}
	for _, collection := range collections(ids) {
		collection.Id = ids[collection.Name]
		if err := app.Save(collection); err != nil {
			t.Fatalf("save %s: %v", collection.Name, err)
		}
	}
	before := schemaFingerprint(t, app)
	if err := Ensure(app); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if after := schemaFingerprint(t, app); after != before {
		t.Fatal("Ensure rewrote valid noncanonical IDs")
	}
}

func TestB_InvalidAuthSecretsRejectWithoutWrites(t *testing.T) {
	setters := []struct {
		name string
		set  func(*core.Collection, string)
	}{
		{"auth", func(c *core.Collection, v string) { c.AuthToken.Secret = v }},
		{"password_reset", func(c *core.Collection, v string) { c.PasswordResetToken.Secret = v }},
		{"email_change", func(c *core.Collection, v string) { c.EmailChangeToken.Secret = v }},
		{"verification", func(c *core.Collection, v string) { c.VerificationToken.Secret = v }},
		{"file", func(c *core.Collection, v string) { c.FileToken.Secret = v }},
	}
	for _, setter := range setters {
		for _, value := range []string{"", "short", strings.Repeat("a", 256)} {
			t.Run(setter.name, func(t *testing.T) {
				// PocketBase validates token secret bounds on Save, so malformed
				// persisted collections cannot be manufactured through its API.
				users := pristineUsers()
				setter.set(users, value)
				if validAuthSecrets(users) {
					t.Fatal("validAuthSecrets accepted malformed secret")
				}
			})
		}
	}
}

func TestB_FreshClassificationRequiresValidAuthSecrets(t *testing.T) {
	app := testApp(t)
	users, _ := app.FindCollectionByNameOrId("users")
	users.AuthToken.Secret = "short"
	snapshot, err := loadCollections(app)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.byName["users"] = users
	state, err := classify(app, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if state != unsupportedSchema {
		t.Fatalf("state = %v, want unsupported", state)
	}
	if _, err := app.FindCollectionByNameOrId("gateway_sessions"); err == nil {
		t.Fatal("classification wrote collections")
	}
}

func TestB_UsersRecordAndModificationRejectWithoutWrites(t *testing.T) {
	for _, modified := range []bool{false, true} {
		t.Run(fmt.Sprintf("modified_%t", modified), func(t *testing.T) {
			app := testApp(t)
			users, _ := app.FindCollectionByNameOrId("users")
			if modified {
				users.Fields.Add(&core.TextField{Name: "unexpected"})
				if err := app.Save(users); err != nil {
					t.Fatal(err)
				}
			} else {
				record := core.NewRecord(users)
				record.Set("email", "present@example.test")
				record.Set("password", "password123")
				record.Set("passwordConfirm", "password123")
				if err := app.Save(record); err != nil {
					t.Fatal(err)
				}
			}
			before := schemaFingerprint(t, app)
			if err := Ensure(app); !errors.Is(err, ErrUnsupportedSchema) {
				t.Fatalf("Ensure = %v", err)
			}
			if after := schemaFingerprint(t, app); after != before {
				t.Fatal("Ensure wrote rejected users state")
			}
		})
	}
}

func TestFreshUsersPhysicalAndRawDriftRejectWithoutWrites(t *testing.T) {
	for _, mutate := range []string{
		"ALTER TABLE users ADD COLUMN intruder TEXT",
		"UPDATE _collections SET options = json_set(options, '$.unknown_option', true) WHERE name = 'users'",
		"UPDATE _collections SET fields = json_set(fields, '$[0].unknown_field', true) WHERE name = 'users'",
	} {
		t.Run(mutate, func(t *testing.T) {
			app := testApp(t)
			if _, err := app.DB().NewQuery(mutate).Execute(); err != nil {
				t.Fatal(err)
			}
			before := fullFingerprint(t, app)
			err := Ensure(app)
			if !errors.Is(err, ErrUnsupportedSchema) {
				t.Fatalf("Ensure = %v", err)
			}
			if after := fullFingerprint(t, app); after != before {
				t.Fatal("Ensure repaired malformed fresh users")
			}
		})
	}
}

func TestRawMetadataUnknownKeysRejectWithoutRepair(t *testing.T) {
	for _, mutate := range []string{
		"UPDATE _collections SET options = json_set(options, '$.unknown_option', true) WHERE name = 'path_policies'",
		"UPDATE _collections SET fields = json_set(fields, '$[0].unknown_field', true) WHERE name = 'path_policies'",
	} {
		t.Run(mutate, func(t *testing.T) {
			app := testApp(t)
			if err := Ensure(app); err != nil {
				t.Fatal(err)
			}
			if _, err := app.DB().NewQuery(mutate).Execute(); err != nil {
				t.Fatal(err)
			}
			before := fullFingerprint(t, app)
			if err := Ensure(app); !errors.Is(err, ErrUnsupportedSchema) {
				t.Fatalf("Ensure = %v", err)
			}
			if after := fullFingerprint(t, app); after != before {
				t.Fatal("Ensure repaired unknown raw JSON key")
			}
		})
	}
}

func TestCollectionNameIDCollisionScope(t *testing.T) {
	first := core.NewBaseCollection("unrelated_one", "unrelated_id")
	second := core.NewBaseCollection("unrelated_id", "another_id")
	snapshot := &collectionSnapshot{byName: map[string]*core.Collection{first.Name: first, second.Name: second}, byID: map[string]*core.Collection{first.Id: first, second.Id: second}}
	if err := rejectRequiredIDNameCollisions(snapshot); err != nil {
		t.Fatalf("unrelated collision rejected: %v", err)
	}
	required := core.NewBaseCollection("unrelated", "gateway_sessions")
	snapshot.byName[required.Name], snapshot.byID[required.Id] = required, required
	if err := rejectRequiredIDNameCollisions(snapshot); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("required collision = %v", err)
	}
	reverse := core.NewBaseCollection("gateway_sessions", "unrelated_id")
	snapshot.byName[reverse.Name] = reverse
	if err := rejectRequiredIDNameCollisions(snapshot); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("required reverse collision = %v", err)
	}
}

func TestFreshIDCollisionRetriesAndRefetchesRelations(t *testing.T) {
	app := testApp(t)
	occupied := core.NewBaseCollection("occupied", "collision_id")
	lock(occupied)
	if err := app.Save(occupied); err != nil {
		t.Fatal(err)
	}
	old := generateApplicationID
	defer func() { generateApplicationID = old }()
	ids := []string{"occupied", "collision_id", "fresh_id_one", "fresh_id_two", "fresh_id_three", "fresh_id_four", "fresh_id_five", "fresh_id_six", "fresh_id_seven", "fresh_id_eight", "fresh_id_nine", "fresh_id_ten"}
	generateApplicationID = func() string { id := ids[0]; ids = ids[1:]; return id }
	if err := Ensure(app); err != nil {
		t.Fatal(err)
	}
	source, _ := app.FindCollectionByNameOrId("upstream_sources")
	endpoint, _ := app.FindCollectionByNameOrId("upstream_endpoints")
	if source.Id == "occupied" || source.Id == "collision_id" || endpoint.Fields.GetByName("source").(*core.RelationField).CollectionId != source.Id {
		t.Fatal("fresh ID retry or persisted relation failed")
	}
}

func TestC_ExtrasAndExistingEnsureAreWriteFree(t *testing.T) {
	app := testApp(t)
	if err := Ensure(app); err != nil {
		t.Fatal(err)
	}
	seedRequiredRows(t, app)
	extra := base("unrelated_collection")
	if err := app.Save(extra); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB().NewQuery("CREATE TABLE raw_unrelated (id TEXT PRIMARY KEY, value TEXT)").Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB().NewQuery("INSERT INTO raw_unrelated VALUES ('1', 'unchanged')").Execute(); err != nil {
		t.Fatal(err)
	}
	var saves atomic.Int32
	updateHook := app.OnCollectionUpdate().BindFunc(func(e *core.CollectionEvent) error {
		saves.Add(1)
		return e.Next()
	})
	createHook := app.OnCollectionCreate().BindFunc(func(e *core.CollectionEvent) error {
		saves.Add(1)
		return e.Next()
	})
	defer app.OnCollectionUpdate().Unbind(updateHook)
	defer app.OnCollectionCreate().Unbind(createHook)
	before := fullFingerprint(t, app)
	if err := Ensure(app); err != nil {
		t.Fatal(err)
	}
	if saves.Load() != 0 {
		t.Fatalf("existing Ensure saved %d collections", saves.Load())
	}
	if after := fullFingerprint(t, app); after != before {
		t.Fatal("existing Ensure wrote metadata, schema, indexes, or records")
	}
	var value string
	if err := app.DB().NewQuery("SELECT value FROM raw_unrelated WHERE id = '1'").Row(&value); err != nil || value != "unchanged" {
		t.Fatalf("raw table changed: %q %v", value, err)
	}
}

func TestD_PhysicalDriftRejectsWithoutWrites(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"extra_column", "ALTER TABLE gateway_sessions ADD COLUMN intruder TEXT"},
		{"missing_column", "ALTER TABLE gateway_sessions DROP COLUMN remote_ip"},
		{"extra_explicit_index", "CREATE INDEX idx_intruder ON gateway_sessions (client)"},
		{"missing_expected_index", "DROP INDEX idx_gateway_sessions_token_hash"},
		{"wrong_same_name_index", "DROP INDEX idx_gateway_sessions_token_hash; CREATE INDEX idx_gateway_sessions_token_hash ON gateway_sessions (client)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := testApp(t)
			if err := Ensure(app); err != nil {
				t.Fatal(err)
			}
			if _, err := app.DB().NewQuery(tc.sql).Execute(); err != nil {
				t.Fatal(err)
			}
			before := fullFingerprint(t, app)
			if err := Ensure(app); !errors.Is(err, ErrUnsupportedSchema) {
				t.Fatalf("Ensure = %v", err)
			}
			if after := fullFingerprint(t, app); after != before {
				t.Fatal("Ensure changed drifted physical schema")
			}
		})
	}
}

func TestD_ColumnComparatorRejectsEveryPhysicalProperty(t *testing.T) {
	want := columnSpec{typ: "TEXT", notNull: true, def: "'X'", pk: 1}
	for _, got := range []columnSpec{
		{typ: "INTEGER", notNull: true, def: "'X'", pk: 1},
		{typ: "TEXT", notNull: false, def: "'X'", pk: 1},
		{typ: "TEXT", notNull: true, def: "'Y'", pk: 1},
		{typ: "TEXT", notNull: true, def: "'X'", pk: 0},
	} {
		if columnSpecsEqual(got, want) {
			t.Fatalf("accepted %#v", got)
		}
	}
}

func TestD_RebuiltTableWrongDeclaredTypeRejectsWithoutWrites(t *testing.T) {
	app := testApp(t)
	if err := Ensure(app); err != nil {
		t.Fatal(err)
	}
	var createSQL string
	if err := app.DB().NewQuery("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'gateway_sessions'").Row(&createSQL); err != nil {
		t.Fatal(err)
	}
	indexes, err := app.TableIndexes("gateway_sessions")
	if err != nil {
		t.Fatal(err)
	}
	changed := regexp.MustCompile("(?i)([`\"]gateway_token_hash[`\"]\\s+)TEXT").ReplaceAllString(createSQL, "${1}INTEGER")
	if changed == createSQL {
		t.Fatalf("did not locate gateway_token_hash declaration in %q", createSQL)
	}
	for _, sql := range []string{
		"ALTER TABLE gateway_sessions RENAME TO gateway_sessions_before_corruption",
		changed,
		"INSERT INTO gateway_sessions SELECT * FROM gateway_sessions_before_corruption",
		"DROP TABLE gateway_sessions_before_corruption",
	} {
		if _, err := app.DB().NewQuery(sql).Execute(); err != nil {
			t.Fatal(err)
		}
	}
	for _, sql := range indexes {
		if _, err := app.DB().NewQuery(sql).Execute(); err != nil {
			t.Fatal(err)
		}
	}
	before := fullFingerprint(t, app)
	if err := Ensure(app); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("Ensure = %v", err)
	}
	if after := fullFingerprint(t, app); after != before {
		t.Fatal("Ensure changed rebuilt corrupt table")
	}
}

func TestD_RebuiltTableConstraintDriftRejectsWithoutWrites(t *testing.T) {
	for _, tc := range []struct{ name, old, new string }{
		{"not_null", "`gateway_token_hash` TEXT DEFAULT '' NOT NULL", "`gateway_token_hash` TEXT DEFAULT ''"},
		{"default", "`gateway_token_hash` TEXT DEFAULT '' NOT NULL", "`gateway_token_hash` TEXT DEFAULT 'wrong' NOT NULL"},
		{"primary_key", "`id` TEXT PRIMARY KEY DEFAULT", "`id` TEXT DEFAULT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := testApp(t)
			if err := Ensure(app); err != nil {
				t.Fatal(err)
			}
			var sql string
			if err := app.DB().NewQuery("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'gateway_sessions'").Row(&sql); err != nil {
				t.Fatal(err)
			}
			changed := strings.Replace(sql, tc.old, tc.new, 1)
			if changed == sql {
				t.Fatalf("missing declaration %q", tc.old)
			}
			indexes, err := app.TableIndexes("gateway_sessions")
			if err != nil {
				t.Fatal(err)
			}
			for _, statement := range []string{"ALTER TABLE gateway_sessions RENAME TO gateway_sessions_before_corruption", changed, "INSERT INTO gateway_sessions SELECT * FROM gateway_sessions_before_corruption", "DROP TABLE gateway_sessions_before_corruption"} {
				if _, err := app.DB().NewQuery(statement).Execute(); err != nil {
					t.Fatal(err)
				}
			}
			for _, statement := range indexes {
				if _, err := app.DB().NewQuery(statement).Execute(); err != nil {
					t.Fatal(err)
				}
			}
			before := fullFingerprint(t, app)
			if err := Ensure(app); !errors.Is(err, ErrUnsupportedSchema) {
				t.Fatalf("Ensure = %v", err)
			}
			if after := fullFingerprint(t, app); after != before {
				t.Fatal("Ensure changed rebuilt constraint drift")
			}
		})
	}
}

func TestD_IndexNormalization(t *testing.T) {
	canonical := `CREATE UNIQUE INDEX "idx" ON "things" ( "one" , [two] ) WHERE "active" = 1`
	equivalent := " create unique index `idx` on [things] (`one`, \"two\") where [active] = 1 "
	if normalizeIndex(canonical) != normalizeIndex(equivalent) {
		t.Fatal("equivalent index spellings differ")
	}
	for _, different := range []string{
		"CREATE INDEX idx ON things (one, two) WHERE active = 1",
		"CREATE UNIQUE INDEX idx ON things (two, one) WHERE active = 1",
		"CREATE UNIQUE INDEX idx ON things (one, two) WHERE active = 0",
	} {
		if normalizeIndex(canonical) == normalizeIndex(different) {
			t.Fatalf("semantic difference normalized away: %s", different)
		}
	}
}

func TestE_MetadataPartialAndTransactionSafety(t *testing.T) {
	for _, mutate := range []struct {
		name string
		fn   func(*core.Collection)
	}{
		{"rule", func(c *core.Collection) { c.ListRule = new(string); *c.ListRule = "id != ''" }},
		{"field_option", func(c *core.Collection) { c.Fields.GetByName("reason").(*core.TextField).Max = 254 }},
		{"index_metadata", func(c *core.Collection) { c.AddIndex("idx_path_policies_wrong", false, "method", "") }},
	} {
		t.Run("metadata_"+mutate.name, func(t *testing.T) {
			app := testApp(t)
			if err := Ensure(app); err != nil {
				t.Fatal(err)
			}
			c, _ := app.FindCollectionByNameOrId("path_policies")
			mutate.fn(c)
			if err := app.Save(c); err != nil {
				t.Fatal(err)
			}
			before := fullFingerprint(t, app)
			if err := Ensure(app); !errors.Is(err, ErrUnsupportedSchema) {
				t.Fatalf("Ensure = %v", err)
			}
			if fullFingerprint(t, app) != before {
				t.Fatal("Ensure changed corrupt metadata")
			}
		})
	}
	for _, names := range [][]string{{"gateway_sessions"}, {"gateway_sessions", "audit_logs"}, {"upstream_endpoints", "path_policies"}} {
		t.Run("partial_"+strings.Join(names, "_"), func(t *testing.T) {
			app := testApp(t)
			for _, name := range names {
				if err := app.Save(base(name)); err != nil {
					t.Fatal(err)
				}
			}
			before := fullFingerprint(t, app)
			if err := Ensure(app); !errors.Is(err, ErrUnsupportedSchema) {
				t.Fatalf("Ensure = %v", err)
			}
			if fullFingerprint(t, app) != before {
				t.Fatal("Ensure changed partial schema")
			}
		})
	}
	t.Run("rollback_and_retry", func(t *testing.T) {
		app := testApp(t)
		var fired atomic.Bool
		id := app.OnCollectionCreate().BindFunc(func(e *core.CollectionEvent) error {
			if e.Collection.Name == "upstream_endpoints" {
				fired.Store(true)
				return errors.New("injected create failure")
			}
			return e.Next()
		})
		if err := Ensure(app); err == nil {
			t.Fatal("Ensure unexpectedly succeeded")
		}
		app.OnCollectionCreate().Unbind(id)
		if !fired.Load() {
			t.Fatal("create hook did not fire")
		}
		if _, err := app.FindCollectionByNameOrId("gateway_sessions"); err == nil {
			t.Fatal("rollback left collection")
		}
		users, _ := app.FindCollectionByNameOrId("users")
		if !collectionEqual(users, pristineUsers()) {
			t.Fatal("rollback changed users")
		}
		if err := Ensure(app); err != nil {
			t.Fatalf("retry: %v", err)
		}
	})
}

func TestE_ConcurrentEnsureCompletesSchema(t *testing.T) {
	app := testApp(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- Ensure(app) }()
	}
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		}
		if err != nil && !strings.Contains(strings.ToLower(err.Error()), "database is locked") && !strings.Contains(strings.ToLower(err.Error()), "database is busy") {
			t.Fatalf("unexpected concurrent Ensure error: %v", err)
		}
		if errors.Is(err, ErrUnsupportedSchema) {
			t.Fatalf("internal race classified unsupported: %v", err)
		}
	}
	if successes == 0 {
		t.Fatal("all concurrent Ensure calls failed")
	}
	if err := Ensure(app); err != nil {
		t.Fatalf("final Ensure: %v", err)
	}
}

func appIDs(t *testing.T, app core.App) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, name := range append([]string{"users"}, requiredNames...) {
		c, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatal(err)
		}
		result[name] = c.Id
	}
	return result
}

func schemaFingerprint(t *testing.T, app core.App) string {
	t.Helper()
	collections, err := app.FindAllCollections()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(collections)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// fullFingerprint deliberately includes all metadata, physical DDL and rows so
// rejected and existing schemas prove that Ensure made no observable write.
func fullFingerprint(t *testing.T, app core.App) string {
	t.Helper()
	parts := []string{schemaFingerprint(t, app)}
	var ddl string
	if err := app.DB().NewQuery("SELECT coalesce(group_concat(entry, '\n'), '') FROM (SELECT type || ':' || name || ':' || coalesce(sql, '') AS entry FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name)").Row(&ddl); err != nil {
		t.Fatal(err)
	}
	parts = append(parts, ddl)
	var rawMetadata string
	if err := app.DB().NewQuery("SELECT coalesce(group_concat(entry, '\n'), '') FROM (SELECT id || ':' || name || ':' || fields || ':' || options AS entry FROM _collections ORDER BY id)").Row(&rawMetadata); err != nil {
		t.Fatal(err)
	}
	parts = append(parts, rawMetadata)
	collections, err := app.FindAllCollections()
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(collections, func(i, j int) bool { return collections[i].Name < collections[j].Name })
	for _, collection := range collections {
		name := collection.Name
		info, err := app.TableInfo(name)
		if err != nil {
			t.Fatal(err)
		}
		indexes, err := app.TableIndexes(name)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(struct {
			Info    any
			Indexes any
		}{info, indexes})
		parts = append(parts, string(data))
		records, err := app.FindRecordsByFilter(name, "", "id", 0, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		data, _ = json.Marshal(records)
		parts = append(parts, string(data))
	}
	return strings.Join(parts, "\n---\n")
}

func seedRequiredRows(t *testing.T, app core.App) {
	t.Helper()
	for _, name := range requiredNames {
		columns, err := app.TableInfo(name)
		if err != nil {
			t.Fatal(err)
		}
		names, values := make([]string, 0, len(columns)), make([]string, 0, len(columns))
		for _, c := range columns {
			if c.NotNull && !c.DefaultValue.Valid {
				names, values = append(names, `"`+c.Name+`"`), append(values, "'seed_"+name+"_"+c.Name+"'")
			}
		}
		if len(names) == 0 {
			continue
		}
		if _, err := app.DB().NewQuery("INSERT INTO \"" + name + "\" (" + strings.Join(names, ",") + ") VALUES (" + strings.Join(values, ",") + ")").Execute(); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
}

func testApp(t *testing.T) core.App {
	t.Helper()
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{DataDir: t.TempDir(), EncryptionEnv: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	return app
}
