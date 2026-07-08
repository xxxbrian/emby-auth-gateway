package pbmigrations

import (
	"context"
	"testing"

	"emby-auth-gateway/internal/pbstore"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func TestGatewayCollectionsAreLockedAndStoreAuthStillWorks(t *testing.T) {
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "test",
	})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	defer app.Cleanup()

	for _, name := range gatewayCollectionNames() {
		collection, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatalf("find collection %s: %v", name, err)
		}
		assertRules(t, collection, nil, nil, nil, nil, nil)
		if name == "gateway_users" && collection.PasswordAuth.Enabled {
			t.Fatal("gateway_users PasswordAuth.Enabled = true, want false")
		}
	}

	users, err := app.FindCollectionByNameOrId("gateway_users")
	if err != nil {
		t.Fatalf("find gateway_users: %v", err)
	}
	record := core.NewRecord(users)
	record.Set("username", "alice")
	record.Set("email", "alice@example.com")
	record.Set("synthetic_user_id", "gateway-user")
	record.Set("enabled", true)
	record.SetPassword("alice-pass")
	if err := app.Save(record); err != nil {
		t.Fatalf("save user: %v", err)
	}

	user, err := pbstore.New(app, nil).AuthenticateGatewayUser(context.Background(), "alice", "alice-pass")
	if err != nil {
		t.Fatalf("authenticate gateway user: %v", err)
	}
	if user.Username != "alice" || user.SyntheticUserID != "gateway-user" {
		t.Fatalf("unexpected authenticated user: %#v", user)
	}
}

func TestGatewayCollectionsLockMigrationDownRestoresRules(t *testing.T) {
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "test",
	})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	defer app.Cleanup()

	if _, err := core.NewMigrationsRunner(app, core.AppMigrations).Down(2); err != nil {
		t.Fatalf("revert phase2 and lock migrations: %v", err)
	}

	users, err := app.FindCollectionByNameOrId("gateway_users")
	if err != nil {
		t.Fatalf("find gateway_users: %v", err)
	}
	assertRules(t, users,
		strPtr("@request.auth.collectionName = 'gateway_users'"),
		strPtr("id = @request.auth.id"),
		strPtr(""),
		strPtr("id = @request.auth.id"),
		strPtr("id = @request.auth.id"),
	)
	if !users.PasswordAuth.Enabled {
		t.Fatal("gateway_users PasswordAuth.Enabled = false, want true after down")
	}

	for _, name := range []string{"emby_servers", "backend_accounts", "user_mappings", "gateway_sessions", "audit_logs"} {
		collection, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatalf("find collection %s: %v", name, err)
		}
		empty := strPtr("")
		assertRules(t, collection, empty, empty, empty, empty, empty)
	}
}

func assertRules(t *testing.T, collection *core.Collection, list, view, create, update, delete *string) {
	t.Helper()
	assertRule(t, collection.Name+" list", collection.ListRule, list)
	assertRule(t, collection.Name+" view", collection.ViewRule, view)
	assertRule(t, collection.Name+" create", collection.CreateRule, create)
	assertRule(t, collection.Name+" update", collection.UpdateRule, update)
	assertRule(t, collection.Name+" delete", collection.DeleteRule, delete)
}

func assertRule(t *testing.T, label string, got, want *string) {
	t.Helper()
	if got == nil || want == nil {
		if got != want {
			t.Fatalf("%s rule = %v, want %v", label, got, want)
		}
		return
	}
	if *got != *want {
		t.Fatalf("%s rule = %q, want %q", label, *got, *want)
	}
}

func strPtr(value string) *string {
	return &value
}
