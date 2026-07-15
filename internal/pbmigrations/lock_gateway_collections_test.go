package pbmigrations

import (
	"context"
	"testing"

	"github.com/xxxbrian/emby-auth-gateway/internal/pbstore"

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
		if name == "users" && collection.PasswordAuth.Enabled {
			t.Fatal("users PasswordAuth.Enabled = true, want false")
		}
	}

	assertFields(t, app, "emby_servers", []string{
		"server_id",
		"server_name",
		"server_version",
		"version_checked_at",
		"backend_user_agent",
		"backend_authorization_client",
		"backend_authorization_device",
		"backend_authorization_device_id",
		"backend_authorization_version",
	}, nil)
	assertFields(t, app, "backend_accounts", []string{"backend_password", "backend_user_id", "backend_token", "token_updated_at"}, []string{"backend_password_encrypted"})
	assertFields(t, app, "gateway_sessions", nil, []string{
		"backend_token",
		"backend_server_id",
		"backend_base_url",
		"backend_user_id",
		"backend_username",
		"backend_user_agent",
		"backend_authorization_client",
		"backend_authorization_device",
		"backend_authorization_device_id",
		"backend_authorization_version",
		"backend_token_encrypted",
	})
	assertTextFieldsOptional(t, app, "emby_servers", backendIdentityFieldNames)
	assertFields(t, app, "user_item_data", []string{"season_id", "run_time_ticks", "orphaned_at", "last_seen_at"}, nil)
	assertFields(t, app, "audit_logs", proxyAuditFieldNames, nil)
	assertTextFieldsOptional(t, app, upstreamSourcesCollection, []string{upstreamAuthGenerationField})

	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users: %v", err)
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

	user, err := pbstore.New(app).AuthenticateGatewayUser(context.Background(), "alice", "alice-pass")
	if err != nil {
		t.Fatalf("authenticate gateway user: %v", err)
	}
	if user.Username != "alice" || user.SyntheticUserID != "gateway-user" {
		t.Fatalf("unexpected authenticated user: %#v", user)
	}
}

func assertTextFieldsOptional(t *testing.T, app core.App, collectionName string, fieldNames []string) {
	t.Helper()
	collection, err := app.FindCollectionByNameOrId(collectionName)
	if err != nil {
		t.Fatalf("find collection %s: %v", collectionName, err)
	}
	for _, name := range fieldNames {
		field, ok := collection.Fields.GetByName(name).(*core.TextField)
		if !ok {
			t.Fatalf("collection %s field %s is %T, want *core.TextField", collectionName, name, collection.Fields.GetByName(name))
		}
		if field.Required {
			t.Fatalf("collection %s field %s is required, want optional", collectionName, name)
		}
	}
}

func assertFields(t *testing.T, app core.App, collectionName string, wantPresent, wantAbsent []string) {
	t.Helper()
	collection, err := app.FindCollectionByNameOrId(collectionName)
	if err != nil {
		t.Fatalf("find collection %s: %v", collectionName, err)
	}
	for _, name := range wantPresent {
		if collection.Fields.GetByName(name) == nil {
			t.Fatalf("collection %s missing field %s", collectionName, name)
		}
	}
	for _, name := range wantAbsent {
		if collection.Fields.GetByName(name) != nil {
			t.Fatalf("collection %s still has field %s", collectionName, name)
		}
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
