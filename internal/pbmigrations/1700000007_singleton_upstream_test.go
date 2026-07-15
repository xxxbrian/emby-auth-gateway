package pbmigrations

import (
	"reflect"
	"slices"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func TestSingletonUpstreamSchema(t *testing.T) {
	app := newMigrationTestApp(t)

	sources := mustCollection(t, app, upstreamSourcesCollection)
	if sources.System {
		t.Fatal("upstream_sources is a system collection")
	}
	assertRules(t, sources, nil, nil, nil, nil, nil)
	assertFieldNames(t, sources, []string{
		"id", "key", "server_id", "server_name", "server_version", "version_checked_at",
		"backend_username", "backend_password", "backend_user_id", "backend_token", "token_updated_at",
		"last_login_at", "last_login_error", "backend_user_agent", "backend_authorization_client",
		"backend_authorization_device", "backend_authorization_device_id", "backend_authorization_version",
		"created", "updated",
	})
	assertSelectField(t, sources, "key", true, 1, []string{"default"})
	assertTextField(t, sources, "server_id", true, 255)
	assertTextField(t, sources, "server_name", false, 255)
	assertTextField(t, sources, "server_version", false, 80)
	assertDateField(t, sources, "version_checked_at", false)
	assertTextField(t, sources, "backend_username", true, 255)
	assertTextField(t, sources, "backend_password", true, 0)
	assertTextField(t, sources, "backend_user_id", false, 80)
	assertTextField(t, sources, "backend_token", false, 0)
	assertDateField(t, sources, "token_updated_at", false)
	assertDateField(t, sources, "last_login_at", false)
	assertTextField(t, sources, "last_login_error", false, 0)
	assertTextField(t, sources, "backend_user_agent", true, 255)
	assertTextField(t, sources, "backend_authorization_client", true, 255)
	assertTextField(t, sources, "backend_authorization_device", true, 255)
	assertTextField(t, sources, "backend_authorization_device_id", true, 255)
	assertTextField(t, sources, "backend_authorization_version", true, 80)
	assertAutodateFields(t, sources)
	assertIndexes(t, sources, []string{
		"CREATE UNIQUE INDEX `idx_upstream_sources_key` ON `upstream_sources` (key)",
	})

	endpoints := mustCollection(t, app, upstreamEndpointsCollection)
	if endpoints.System {
		t.Fatal("upstream_endpoints is a system collection")
	}
	assertRules(t, endpoints, nil, nil, nil, nil, nil)
	assertFieldNames(t, endpoints, []string{"id", "source", "key", "base_url", "active", "created", "updated"})
	relation, ok := endpoints.Fields.GetByName("source").(*core.RelationField)
	if !ok || !relation.Required || relation.MaxSelect != 1 || relation.CascadeDelete || relation.CollectionId != sources.Id {
		t.Fatalf("unexpected source relation: %#v", endpoints.Fields.GetByName("source"))
	}
	assertTextField(t, endpoints, "key", true, 80)
	if field, ok := endpoints.Fields.GetByName("base_url").(*core.URLField); !ok || !field.Required {
		t.Fatalf("unexpected base_url field: %#v", endpoints.Fields.GetByName("base_url"))
	}
	if field, ok := endpoints.Fields.GetByName("active").(*core.BoolField); !ok || field.Required {
		t.Fatalf("unexpected active field: %#v", endpoints.Fields.GetByName("active"))
	}
	assertAutodateFields(t, endpoints)
	assertIndexes(t, endpoints, []string{
		"CREATE UNIQUE INDEX `idx_upstream_endpoints_source_key` ON `upstream_endpoints` (source, key)",
		"CREATE UNIQUE INDEX `idx_upstream_endpoints_source_base_url` ON `upstream_endpoints` (source, base_url)",
		"CREATE UNIQUE INDEX `idx_upstream_endpoints_active_source` ON `upstream_endpoints` (source) WHERE active = 1",
	})
}

func TestSingletonUpstreamConstraintsAndRelations(t *testing.T) {
	app := newMigrationTestApp(t)
	sources := mustCollection(t, app, upstreamSourcesCollection)
	endpoints := mustCollection(t, app, upstreamEndpointsCollection)

	invalidSource := newSourceRecord(sources, "other")
	if err := app.Save(invalidSource); err == nil {
		t.Fatal("saved source with invalid key")
	}

	source := newSourceRecord(sources, "default")
	if err := app.Save(source); err != nil {
		t.Fatalf("save source: %v", err)
	}
	if err := app.Save(newSourceRecord(sources, "default")); err == nil {
		t.Fatal("saved second default source")
	}

	inactiveA := newEndpointRecord(endpoints, source.Id, "a", "https://a.example.test", false)
	inactiveB := newEndpointRecord(endpoints, source.Id, "b", "https://b.example.test", false)
	if err := app.Save(inactiveA); err != nil {
		t.Fatalf("save inactive endpoint a: %v", err)
	}
	if err := app.Save(inactiveB); err != nil {
		t.Fatalf("save inactive endpoint b: %v", err)
	}

	active := newEndpointRecord(endpoints, source.Id, "active-a", "https://active-a.example.test", true)
	if err := app.Save(active); err != nil {
		t.Fatalf("save active endpoint: %v", err)
	}
	if err := app.Save(newEndpointRecord(endpoints, source.Id, "active-b", "https://active-b.example.test", true)); err == nil {
		t.Fatal("saved second active endpoint")
	}

	if err := app.RunInTransaction(func(txApp core.App) error {
		active.Set("active", false)
		if err := txApp.Save(active); err != nil {
			return err
		}
		inactiveA.Set("active", true)
		return txApp.Save(inactiveA)
	}); err != nil {
		t.Fatalf("switch active endpoint transaction: %v", err)
	}

	if err := app.Delete(inactiveB); err != nil {
		t.Fatalf("delete endpoint: %v", err)
	}
	if _, err := app.FindRecordById(upstreamSourcesCollection, source.Id); err != nil {
		t.Fatalf("source was deleted with endpoint: %v", err)
	}
}

func TestSingletonUpstreamDownMigration(t *testing.T) {
	t.Run("refuses populated collections", func(t *testing.T) {
		app := newMigrationTestApp(t)
		source := newSourceRecord(mustCollection(t, app, upstreamSourcesCollection), "default")
		if err := app.Save(source); err != nil {
			t.Fatalf("save source: %v", err)
		}
		if err := upstreamCollectionsDown(app); err == nil {
			t.Fatal("down migration succeeded with source records")
		}
		if _, err := app.FindCollectionByNameOrId(upstreamSourcesCollection); err != nil {
			t.Fatalf("source collection removed after refused down migration: %v", err)
		}
		if _, err := app.FindCollectionByNameOrId(upstreamEndpointsCollection); err != nil {
			t.Fatalf("endpoint collection removed after refused down migration: %v", err)
		}
	})

	t.Run("removes empty collections endpoint first", func(t *testing.T) {
		app := newMigrationTestApp(t)
		if err := upstreamCollectionsDown(app); err != nil {
			t.Fatalf("down migration: %v", err)
		}
		if _, err := app.FindCollectionByNameOrId(upstreamEndpointsCollection); err == nil {
			t.Fatal("endpoint collection still exists")
		}
		if _, err := app.FindCollectionByNameOrId(upstreamSourcesCollection); err == nil {
			t.Fatal("source collection still exists")
		}
	})
}

func TestSingletonUpstreamBootstrapAndCompatibility(t *testing.T) {
	app := newMigrationTestApp(t)
	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("repeat migrations: %v", err)
	}
	if err := upstreamCollectionsUp(app); err != nil {
		t.Fatalf("accept compatible existing collections: %v", err)
	}

	sources := mustCollection(t, app, upstreamSourcesCollection)
	sources.Fields.GetByName("server_id").(*core.TextField).Max = 80
	if err := app.Save(sources); err != nil {
		t.Fatalf("save incompatible source collection: %v", err)
	}
	if err := upstreamCollectionsUp(app); err == nil {
		t.Fatal("accepted incompatible existing source collection")
	}
}

func TestSingletonUpstreamRejectsIncompatibleExistingCollections(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, app *tests.TestApp)
		check  func(t *testing.T, app *tests.TestApp)
	}{
		{
			name: "source system collection",
			mutate: func(t *testing.T, app *tests.TestApp) {
				t.Helper()
				sources := mustCollection(t, app, upstreamSourcesCollection)
				sources.System = true
				if err := app.SaveNoValidate(sources); err != nil {
					t.Fatalf("save incompatible source collection: %v", err)
				}
			},
			check: func(t *testing.T, app *tests.TestApp) {
				t.Helper()
				if !mustCollection(t, app, upstreamSourcesCollection).System {
					t.Fatal("migration replaced incompatible source collection")
				}
			},
		},
		{
			name: "endpoint system collection",
			mutate: func(t *testing.T, app *tests.TestApp) {
				t.Helper()
				endpoints := mustCollection(t, app, upstreamEndpointsCollection)
				endpoints.System = true
				if err := app.SaveNoValidate(endpoints); err != nil {
					t.Fatalf("save incompatible endpoint collection: %v", err)
				}
			},
			check: func(t *testing.T, app *tests.TestApp) {
				t.Helper()
				if !mustCollection(t, app, upstreamEndpointsCollection).System {
					t.Fatal("migration replaced incompatible endpoint collection")
				}
			},
		},
		{
			name: "endpoint relation target",
			mutate: func(t *testing.T, app *tests.TestApp) {
				t.Helper()
				endpoints := mustCollection(t, app, upstreamEndpointsCollection)
				endpoints.Fields.GetByName("source").(*core.RelationField).CollectionId = mustCollection(t, app, "users").Id
				if err := app.SaveNoValidate(endpoints); err != nil {
					t.Fatalf("save incompatible endpoint relation: %v", err)
				}
			},
			check: func(t *testing.T, app *tests.TestApp) {
				t.Helper()
				relation := mustCollection(t, app, upstreamEndpointsCollection).Fields.GetByName("source").(*core.RelationField)
				if relation.CollectionId != mustCollection(t, app, "users").Id {
					t.Fatal("migration replaced incompatible endpoint relation")
				}
			},
		},
		{
			name: "endpoint cascade delete",
			mutate: func(t *testing.T, app *tests.TestApp) {
				t.Helper()
				endpoints := mustCollection(t, app, upstreamEndpointsCollection)
				endpoints.Fields.GetByName("source").(*core.RelationField).CascadeDelete = true
				if err := app.Save(endpoints); err != nil {
					t.Fatalf("save incompatible endpoint cascade relation: %v", err)
				}
			},
			check: func(t *testing.T, app *tests.TestApp) {
				t.Helper()
				if !mustCollection(t, app, upstreamEndpointsCollection).Fields.GetByName("source").(*core.RelationField).CascadeDelete {
					t.Fatal("migration replaced incompatible endpoint cascade relation")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app := newMigrationTestApp(t)
			tc.mutate(t, app)
			if err := upstreamCollectionsUp(app); err == nil {
				t.Fatal("accepted incompatible existing collections")
			}
			tc.check(t, app)
		})
	}
}

func newMigrationTestApp(t *testing.T) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{DataDir: t.TempDir(), EncryptionEnv: "test"})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	t.Cleanup(app.Cleanup)
	return app
}

func mustCollection(t *testing.T, app core.App, name string) *core.Collection {
	t.Helper()
	collection, err := app.FindCollectionByNameOrId(name)
	if err != nil {
		t.Fatalf("find collection %s: %v", name, err)
	}
	return collection
}

func newSourceRecord(collection *core.Collection, key string) *core.Record {
	record := core.NewRecord(collection)
	record.Set("key", key)
	record.Set("server_id", "server")
	record.Set("backend_username", "backend")
	record.Set("backend_password", "password")
	record.Set("backend_user_agent", "agent")
	record.Set("backend_authorization_client", "client")
	record.Set("backend_authorization_device", "device")
	record.Set("backend_authorization_device_id", "device-id")
	record.Set("backend_authorization_version", "1.0")
	return record
}

func newEndpointRecord(collection *core.Collection, sourceID, key, baseURL string, active bool) *core.Record {
	record := core.NewRecord(collection)
	record.Set("source", sourceID)
	record.Set("key", key)
	record.Set("base_url", baseURL)
	record.Set("active", active)
	return record
}

func assertTextField(t *testing.T, collection *core.Collection, name string, required bool, max int) {
	t.Helper()
	field, ok := collection.Fields.GetByName(name).(*core.TextField)
	if !ok || field.Required != required || field.Max != max {
		t.Fatalf("unexpected %s field: %#v", name, collection.Fields.GetByName(name))
	}
}

func assertSelectField(t *testing.T, collection *core.Collection, name string, required bool, maxSelect int, values []string) {
	t.Helper()
	field, ok := collection.Fields.GetByName(name).(*core.SelectField)
	if !ok || field.Required != required || field.MaxSelect != maxSelect || !reflect.DeepEqual(field.Values, values) {
		t.Fatalf("unexpected %s field: %#v", name, collection.Fields.GetByName(name))
	}
}

func assertDateField(t *testing.T, collection *core.Collection, name string, required bool) {
	t.Helper()
	field, ok := collection.Fields.GetByName(name).(*core.DateField)
	if !ok || field.Required != required {
		t.Fatalf("unexpected %s field: %#v", name, collection.Fields.GetByName(name))
	}
}

func assertFieldNames(t *testing.T, collection *core.Collection, want []string) {
	t.Helper()
	got := make([]string, 0, len(collection.Fields))
	for _, field := range collection.Fields {
		got = append(got, field.GetName())
	}
	if !slices.Equal(got, want) {
		t.Fatalf("collection %s fields = %#v, want %#v", collection.Name, got, want)
	}
}

func assertAutodateFields(t *testing.T, collection *core.Collection) {
	t.Helper()
	created, createdOK := collection.Fields.GetByName("created").(*core.AutodateField)
	updated, updatedOK := collection.Fields.GetByName("updated").(*core.AutodateField)
	if !createdOK || !created.OnCreate || created.OnUpdate || !updatedOK || !updated.OnCreate || !updated.OnUpdate {
		t.Fatalf("unexpected autodate fields: created=%#v updated=%#v", created, updated)
	}
}

func assertIndexes(t *testing.T, collection *core.Collection, want []string) {
	t.Helper()
	if len(collection.Indexes) != len(want) {
		t.Fatalf("collection %s indexes = %#v, want %#v", collection.Name, collection.Indexes, want)
	}
	for i, index := range want {
		if collection.Indexes[i] != index {
			t.Fatalf("collection %s index %d = %q, want %q", collection.Name, i, collection.Indexes[i], index)
		}
	}
}
