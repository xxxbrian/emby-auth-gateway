package pbmigrations

import (
	"slices"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func TestUpstreamAuthGenerationFreshBootstrap(t *testing.T) {
	app := newMigrationTestApp(t)
	sources := mustCollection(t, app, upstreamSourcesCollection)

	assertTextField(t, sources, upstreamAuthGenerationField, false, 128)
	assertFieldNames(t, sources, []string{
		"id", "key", "server_id", "server_name", "server_version", "version_checked_at",
		"backend_username", "backend_password", "backend_user_id", "backend_token", "token_updated_at",
		"last_login_at", "last_login_error", "backend_user_agent", "backend_authorization_client",
		"backend_authorization_device", "backend_authorization_device_id", "backend_authorization_version",
		"created", "updated", "auth_generation_id",
	})
}

func TestUpstreamAuthGenerationUpgrades1700000007Schema(t *testing.T) {
	app := newMigrationTestApp(t)
	if err := upstreamAuthGenerationDown(app); err != nil {
		t.Fatalf("restore 1700000007 schema: %v", err)
	}
	if err := upstreamCollectionsUp(app); err != nil {
		t.Fatalf("bootstrap 1700000007 schema: %v", err)
	}

	sources := mustCollection(t, app, upstreamSourcesCollection)
	source := newSourceRecord(sources, "default")
	source.Set("server_name", "Existing source")
	if err := app.Save(source); err != nil {
		t.Fatalf("save source: %v", err)
	}
	fieldNames := sourceFieldNames(sources)
	indexes := slices.Clone(sources.Indexes)

	if err := upstreamAuthGenerationUp(app); err != nil {
		t.Fatalf("upgrade schema: %v", err)
	}

	sources = mustCollection(t, app, upstreamSourcesCollection)
	if got := sourceFieldNames(sources); !slices.Equal(got, append(fieldNames, upstreamAuthGenerationField)) {
		t.Fatalf("source fields = %#v, want existing fields plus %q", got, upstreamAuthGenerationField)
	}
	if !slices.Equal(sources.Indexes, indexes) {
		t.Fatalf("source indexes = %#v, want %#v", sources.Indexes, indexes)
	}
	reloaded, err := app.FindRecordById(upstreamSourcesCollection, source.Id)
	if err != nil {
		t.Fatalf("find existing source: %v", err)
	}
	if reloaded.GetString("server_name") != "Existing source" || reloaded.GetString(upstreamAuthGenerationField) != "" {
		t.Fatalf("existing source changed by upgrade: %#v", reloaded.PublicExport())
	}
}

func TestUpstreamAuthGenerationCompatibility(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*core.Collection)
	}{
		{
			name: "wrong type",
			mutate: func(sources *core.Collection) {
				sources.Fields.RemoveByName(upstreamAuthGenerationField)
				sources.Fields.Add(&core.BoolField{Name: upstreamAuthGenerationField})
			},
		},
		{
			name: "required",
			mutate: func(sources *core.Collection) {
				sources.Fields.GetByName(upstreamAuthGenerationField).(*core.TextField).Required = true
			},
		},
		{
			name: "wrong max",
			mutate: func(sources *core.Collection) {
				sources.Fields.GetByName(upstreamAuthGenerationField).(*core.TextField).Max = 80
			},
		},
	}

	t.Run("repeated compatible application", func(t *testing.T) {
		app := newMigrationTestApp(t)
		for range 2 {
			if err := upstreamAuthGenerationUp(app); err != nil {
				t.Fatalf("repeat compatible migration: %v", err)
			}
		}
	})

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app := newMigrationTestApp(t)
			sources := mustCollection(t, app, upstreamSourcesCollection)
			tc.mutate(sources)
			if err := app.SaveNoValidate(sources); err != nil {
				t.Fatalf("save incompatible collection: %v", err)
			}
			if err := upstreamAuthGenerationUp(app); err == nil {
				t.Fatal("accepted incompatible auth generation field")
			}
		})
	}
}

func TestUpstreamAuthGenerationDownMigration(t *testing.T) {
	t.Run("removes empty field", func(t *testing.T) {
		app := newMigrationTestApp(t)
		sources := mustCollection(t, app, upstreamSourcesCollection)
		source := newSourceRecord(sources, "default")
		if err := app.Save(source); err != nil {
			t.Fatalf("save source: %v", err)
		}
		if err := upstreamAuthGenerationDown(app); err != nil {
			t.Fatalf("down migration: %v", err)
		}
		if field := mustCollection(t, app, upstreamSourcesCollection).Fields.GetByName(upstreamAuthGenerationField); field != nil {
			t.Fatalf("auth generation field still exists: %#v", field)
		}
		if _, err := app.FindRecordById(upstreamSourcesCollection, source.Id); err != nil {
			t.Fatalf("source removed by down migration: %v", err)
		}
	})

	t.Run("refuses populated generation", func(t *testing.T) {
		app := newMigrationTestApp(t)
		sources := mustCollection(t, app, upstreamSourcesCollection)
		source := newSourceRecord(sources, "default")
		source.Set(upstreamAuthGenerationField, "generation-1")
		if err := app.Save(source); err != nil {
			t.Fatalf("save source: %v", err)
		}

		if err := upstreamAuthGenerationDown(app); err == nil {
			t.Fatal("down migration succeeded with auth generation")
		}
		sources = mustCollection(t, app, upstreamSourcesCollection)
		assertTextField(t, sources, upstreamAuthGenerationField, false, 128)
		reloaded, err := app.FindRecordById(upstreamSourcesCollection, source.Id)
		if err != nil {
			t.Fatalf("find source after refused down migration: %v", err)
		}
		if got := reloaded.GetString(upstreamAuthGenerationField); got != "generation-1" {
			t.Fatalf("auth generation after refused down migration = %q", got)
		}
	})
}

func sourceFieldNames(collection *core.Collection) []string {
	names := make([]string, 0, len(collection.Fields))
	for _, field := range collection.Fields {
		names = append(names, field.GetName())
	}
	return names
}
