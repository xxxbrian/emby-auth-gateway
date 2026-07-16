package compat

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	_ "github.com/xxxbrian/emby-auth-gateway/internal/pbmigrations"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
)

func TestCurrentFinalMigrationSchemaIsAccepted(t *testing.T) {
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{DataDir: t.TempDir(), EncryptionEnv: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	if err := app.RunAppMigrations(); err != nil {
		t.Fatalf("run registered application migrations: %v", err)
	}
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(users)
	record.Set("username", "durable")
	record.Set("email", "durable@example.test")
	record.Set("synthetic_user_id", "durable-user")
	record.SetPassword("durable-password")
	if err := app.Save(record); err != nil {
		t.Fatalf("seed durable row: %v", err)
	}
	before := snapshot(t, app)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if after := snapshot(t, app); after != before {
		t.Fatal("Ensure rewrote migrated schema or durable rows")
	}
}

func snapshot(t *testing.T, app core.App) string {
	t.Helper()
	collections, err := app.FindAllCollections()
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(collections, func(i, j int) bool { return collections[i].Name < collections[j].Name })
	data, err := json.Marshal(collections)
	if err != nil {
		t.Fatal(err)
	}
	parts := []string{string(data)}
	for _, query := range []string{
		"SELECT coalesce(group_concat(entry, '\n'), '') FROM (SELECT id || ':' || name || ':' || fields || ':' || options AS entry FROM _collections ORDER BY id)",
		"SELECT coalesce(group_concat(entry, '\n'), '') FROM (SELECT type || ':' || name || ':' || coalesce(sql, '') AS entry FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name)",
		"SELECT coalesce(group_concat(file || ':' || applied, '\n'), '') FROM _migrations ORDER BY file",
	} {
		var value string
		if err := app.DB().NewQuery(query).Row(&value); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, value)
	}
	for _, collection := range collections {
		info, err := app.TableInfo(collection.Name)
		if err != nil {
			t.Fatal(err)
		}
		indexes, err := app.TableIndexes(collection.Name)
		if err != nil {
			t.Fatal(err)
		}
		physical, _ := json.Marshal(struct {
			Info    any
			Indexes any
		}{info, indexes})
		parts = append(parts, string(physical))
		records, err := app.FindRecordsByFilter(collection.Name, "", "id", 0, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		rows, _ := json.Marshal(records)
		parts = append(parts, string(rows))
	}
	return strings.Join(parts, "\n---\n")
}
