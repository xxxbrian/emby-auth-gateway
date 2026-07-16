package pbsetup

import (
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
)

// newServer seeds a legacy emby_servers record for import tests.
func newServer(t *testing.T, app core.App, name, baseURL string) *core.Record {
	t.Helper()
	collection, err := app.FindCollectionByNameOrId("emby_servers")
	if err != nil {
		t.Fatalf("find servers collection: %v", err)
	}
	server := core.NewRecord(collection)
	server.Set("name", name)
	server.Set("base_url", baseURL)
	identity := gateway.DefaultBackendClientIdentity()
	server.Set("backend_user_agent", identity.UserAgent)
	server.Set("backend_authorization_client", identity.Client)
	server.Set("backend_authorization_device", identity.Device)
	server.Set("backend_authorization_device_id", "00000000-0000-4000-8000-000000000001")
	server.Set("backend_authorization_version", identity.Version)
	server.Set("enabled", true)
	if err := app.Save(server); err != nil {
		t.Fatalf("save server: %v", err)
	}
	return server
}

// newBackendAccount seeds a legacy backend_accounts record for import tests.
func newBackendAccount(t *testing.T, app core.App, name, serverID, username, password string, enabled bool, authenticatedAt time.Time) *core.Record {
	t.Helper()
	collection, err := app.FindCollectionByNameOrId("backend_accounts")
	if err != nil {
		t.Fatalf("find backend accounts collection: %v", err)
	}
	account := core.NewRecord(collection)
	account.Set("name", name)
	account.Set("server", serverID)
	account.Set("backend_username", username)
	account.Set("backend_password", password)
	account.Set("enabled", enabled)
	if !authenticatedAt.IsZero() {
		account.Set("backend_user_id", "upstream-user")
		account.Set("backend_token", "upstream-token")
		account.Set("token_updated_at", authenticatedAt)
		account.Set("last_login_at", authenticatedAt)
		account.Set("last_login_error", "previous failure")
	}
	if err := app.Save(account); err != nil {
		t.Fatalf("save backend account: %v", err)
	}
	return account
}

func seedLegacyImportRecords(t *testing.T, app core.App, serverName, baseURL, accountName, backendUsername, backendPassword string) (*core.Record, *core.Record) {
	t.Helper()
	server := newServer(t, app, serverName, baseURL)
	account := newBackendAccount(t, app, accountName, server.Id, backendUsername, backendPassword, true, time.Time{})
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users collection: %v", err)
	}
	user := core.NewRecord(users)
	username := "gateway"
	user.Set("username", username)
	user.SetEmail(internalEmail(username))
	user.SetPassword("password")
	user.Set("synthetic_user_id", "legacy-synthetic")
	user.Set("enabled", true)
	if err := app.Save(user); err != nil {
		t.Fatalf("save legacy user: %v", err)
	}
	mappings, err := app.FindCollectionByNameOrId("user_mappings")
	if err != nil {
		t.Fatalf("find mappings collection: %v", err)
	}
	mapping := core.NewRecord(mappings)
	mapping.Set("gateway_user", user.Id)
	mapping.Set("backend_account", account.Id)
	mapping.Set("enabled", true)
	if err := app.Save(mapping); err != nil {
		t.Fatalf("save legacy mapping: %v", err)
	}
	return server, account
}
