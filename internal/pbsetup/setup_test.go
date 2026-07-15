package pbsetup

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
	_ "github.com/xxxbrian/emby-auth-gateway/internal/pbmigrations"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/hook"
)

func TestSetupWritesPlainBackendPasswordAndDefaultClientIdentity(t *testing.T) {
	app := newTestApp(t)
	opts := options{
		GatewayUsername:    "alice",
		GatewayPassword:    "gateway-pass",
		SyntheticUserID:    "gateway-alice",
		EmbyServerName:     "server",
		EmbyBaseURL:        "https://emby.example.com/emby/",
		BackendAccountName: "backend",
		BackendUsername:    "real-alice",
		BackendPassword:    "backend-pass",
	}

	if err := run(app, opts); err != nil {
		t.Fatalf("run setup: %v", err)
	}

	server, err := app.FindFirstRecordByData("emby_servers", "name", "server")
	if err != nil {
		t.Fatalf("find server: %v", err)
	}
	defaults := gateway.DefaultBackendClientIdentity()
	deviceID := server.GetString("backend_authorization_device_id")
	if server.GetString("base_url") != "https://emby.example.com/emby" || server.GetString("backend_user_agent") != defaults.UserAgent || server.GetString("backend_authorization_client") != defaults.Client || server.GetString("backend_authorization_device") != defaults.Device || !uuidPattern.MatchString(deviceID) || server.GetString("backend_authorization_version") != defaults.Version {
		t.Fatalf("unexpected server identity: %#v", server)
	}
	if err := run(app, opts); err != nil {
		t.Fatalf("run setup update: %v", err)
	}
	updated, err := app.FindFirstRecordByData("emby_servers", "name", "server")
	if err != nil {
		t.Fatalf("find updated server: %v", err)
	}
	if updated.GetString("backend_authorization_device_id") != deviceID {
		t.Fatalf("device id changed on setup update: got %q want %q", updated.GetString("backend_authorization_device_id"), deviceID)
	}

	account, err := app.FindFirstRecordByData("backend_accounts", "name", "backend")
	if err != nil {
		t.Fatalf("find backend account: %v", err)
	}
	if account.GetString("backend_password") != "backend-pass" {
		t.Fatalf("backend_password = %q, want plaintext backend-pass", account.GetString("backend_password"))
	}
}

func TestSetupClearsBackendTokenWhenCredentialsChange(t *testing.T) {
	app := newTestApp(t)
	opts := options{
		GatewayUsername:    "alice",
		GatewayPassword:    "gateway-pass",
		SyntheticUserID:    "gateway-alice",
		EmbyServerName:     "server",
		EmbyBaseURL:        "https://emby.example.com",
		BackendAccountName: "backend",
		BackendUsername:    "real-alice",
		BackendPassword:    "backend-pass",
	}
	if err := run(app, opts); err != nil {
		t.Fatalf("run setup: %v", err)
	}
	account, err := app.FindFirstRecordByData("backend_accounts", "name", "backend")
	if err != nil {
		t.Fatalf("find account: %v", err)
	}
	account.Set("backend_user_id", "backend-user")
	account.Set("backend_token", "backend-token")
	account.Set("token_updated_at", time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC))
	account.Set("last_login_at", time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC))
	account.Set("last_login_error", "previous failure")
	if err := app.Save(account); err != nil {
		t.Fatalf("save account token: %v", err)
	}
	if err := run(app, opts); err != nil {
		t.Fatalf("run setup same credentials: %v", err)
	}
	account, err = app.FindFirstRecordByData("backend_accounts", "name", "backend")
	if err != nil {
		t.Fatalf("find unchanged account: %v", err)
	}
	if account.GetString("backend_token") != "backend-token" || account.GetString("backend_user_id") != "backend-user" || account.GetDateTime("token_updated_at").IsZero() || account.GetDateTime("last_login_at").IsZero() || account.GetString("last_login_error") != "previous failure" {
		t.Fatalf("auth state was cleared without credential change: %#v", account)
	}

	opts.BackendPassword = "new-backend-pass"
	if err := run(app, opts); err != nil {
		t.Fatalf("run setup changed credentials: %v", err)
	}
	account, err = app.FindFirstRecordByData("backend_accounts", "name", "backend")
	if err != nil {
		t.Fatalf("find changed account: %v", err)
	}
	if account.GetString("backend_token") != "" || account.GetString("backend_user_id") != "" || !account.GetDateTime("token_updated_at").IsZero() || !account.GetDateTime("last_login_at").IsZero() || account.GetString("last_login_error") != "" {
		t.Fatalf("backend token state was not cleared: %#v", account)
	}
}

func TestSetupResetsLinkedAccountAuthStateOnlyWhenBaseURLChanges(t *testing.T) {
	app := newTestApp(t)
	opts := options{
		GatewayUsername:    "alice",
		GatewayPassword:    "gateway-pass",
		SyntheticUserID:    "gateway-alice",
		EmbyServerName:     "server",
		EmbyBaseURL:        "https://emby.example.com/emby",
		BackendAccountName: "backend",
		BackendUsername:    "real-alice",
		BackendPassword:    "backend-pass",
	}
	if err := run(app, opts); err != nil {
		t.Fatalf("run setup: %v", err)
	}

	server, err := app.FindFirstRecordByData("emby_servers", "name", opts.EmbyServerName)
	if err != nil {
		t.Fatalf("find server: %v", err)
	}
	checkedAt := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	server.Set("server_id", "upstream-server")
	server.Set("server_name", "Upstream Emby")
	server.Set("server_version", "4.9.0")
	server.Set("version_checked_at", checkedAt)
	server.Set("backend_authorization_device_id", "preserved-device-id")
	if err := app.Save(server); err != nil {
		t.Fatalf("save server identity: %v", err)
	}
	account, err := app.FindFirstRecordByData("backend_accounts", "name", opts.BackendAccountName)
	if err != nil {
		t.Fatalf("find backend account: %v", err)
	}
	setBackendAccountAuthState(account, checkedAt)
	if err := app.Save(account); err != nil {
		t.Fatalf("save backend account: %v", err)
	}
	linkedAccount := newBackendAccount(t, app, "linked", server.Id, "linked-user", "linked-password", false, checkedAt)
	otherServer := newServer(t, app, "other-server", "https://other.example.com")
	otherAccount := newBackendAccount(t, app, "other", otherServer.Id, "other-user", "other-password", false, checkedAt)

	assertServerIdentity := func(t *testing.T, wantPresent bool) {
		t.Helper()
		server, err = app.FindFirstRecordByData("emby_servers", "name", opts.EmbyServerName)
		if err != nil {
			t.Fatalf("find server: %v", err)
		}
		if wantPresent && (server.GetString("server_id") != "upstream-server" || server.GetString("server_name") != "Upstream Emby" || server.GetString("server_version") != "4.9.0" || !server.GetDateTime("version_checked_at").Time().Equal(checkedAt)) {
			t.Fatalf("server identity was not preserved: %#v", server)
		}
		if !wantPresent && (server.GetString("server_id") != "" || server.GetString("server_name") != "" || server.GetString("server_version") != "" || !server.GetDateTime("version_checked_at").IsZero()) {
			t.Fatalf("server identity was not cleared: %#v", server)
		}
		if server.GetString("backend_authorization_device_id") != "preserved-device-id" || !server.GetBool("enabled") {
			t.Fatalf("unrelated server fields changed: %#v", server)
		}
	}
	assertAccount := func(t *testing.T, name, serverID, username, password string, enabled, wantAuth bool) {
		t.Helper()
		account, err := app.FindFirstRecordByData("backend_accounts", "name", name)
		if err != nil {
			t.Fatalf("find account %q: %v", name, err)
		}
		if account.GetString("server") != serverID || account.GetString("backend_username") != username || account.GetString("backend_password") != password || account.GetBool("enabled") != enabled {
			t.Fatalf("account configuration changed: %#v", account)
		}
		assertBackendAccountAuthState(t, account, checkedAt, wantAuth)
	}

	if err := run(app, opts); err != nil {
		t.Fatalf("run setup unchanged URL: %v", err)
	}
	assertServerIdentity(t, true)
	assertAccount(t, "backend", server.Id, "real-alice", "backend-pass", true, true)
	assertAccount(t, linkedAccount.GetString("name"), server.Id, "linked-user", "linked-password", false, true)
	assertAccount(t, otherAccount.GetString("name"), otherServer.Id, "other-user", "other-password", false, true)

	opts.EmbyBaseURL += "/"
	if err := run(app, opts); err != nil {
		t.Fatalf("run setup normalization-equivalent URL: %v", err)
	}
	assertServerIdentity(t, true)
	assertAccount(t, "backend", server.Id, "real-alice", "backend-pass", true, true)
	assertAccount(t, linkedAccount.GetString("name"), server.Id, "linked-user", "linked-password", false, true)

	opts.EmbyBaseURL = "https://new-emby.example.com/emby"
	if err := run(app, opts); err != nil {
		t.Fatalf("run setup changed URL: %v", err)
	}
	assertServerIdentity(t, false)
	assertAccount(t, "backend", server.Id, "real-alice", "backend-pass", true, false)
	assertAccount(t, linkedAccount.GetString("name"), server.Id, "linked-user", "linked-password", false, false)
	assertAccount(t, otherAccount.GetString("name"), otherServer.Id, "other-user", "other-password", false, true)
}

func TestSetupRollsBackBaseURLAndLinkedAccountInvalidationOnLaterFailure(t *testing.T) {
	app := newTestApp(t)
	opts := options{GatewayUsername: "alice", GatewayPassword: "gateway-pass", SyntheticUserID: "gateway-alice", EmbyServerName: "server", EmbyBaseURL: "https://emby.example.com", BackendAccountName: "backend", BackendUsername: "real-alice", BackendPassword: "backend-pass"}
	if err := run(app, opts); err != nil {
		t.Fatalf("run setup: %v", err)
	}
	server, err := app.FindFirstRecordByData("emby_servers", "name", opts.EmbyServerName)
	if err != nil {
		t.Fatalf("find server: %v", err)
	}
	checkedAt := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	server.Set("server_id", "upstream-server")
	server.Set("server_name", "Upstream Emby")
	server.Set("server_version", "4.9.0")
	server.Set("version_checked_at", checkedAt)
	if err := app.Save(server); err != nil {
		t.Fatalf("save server: %v", err)
	}
	account, err := app.FindFirstRecordByData("backend_accounts", "name", opts.BackendAccountName)
	if err != nil {
		t.Fatalf("find account: %v", err)
	}
	setBackendAccountAuthState(account, checkedAt)
	if err := app.Save(account); err != nil {
		t.Fatalf("save account: %v", err)
	}
	linkedAccount := newBackendAccount(t, app, "linked", server.Id, "linked-user", "linked-password", true, checkedAt)
	mappings, err := app.FindCollectionByNameOrId("user_mappings")
	if err != nil {
		t.Fatalf("find mappings collection: %v", err)
	}
	app.OnRecordUpdateExecute(mappings.Id).Bind(&hook.Handler[*core.RecordEvent]{
		Id:       "fail_mapping_save",
		Priority: 9999999999,
		Func: func(*core.RecordEvent) error {
			return errors.New("mapping save failed")
		},
	})
	t.Cleanup(func() { app.OnRecordUpdateExecute(mappings.Id).Unbind("fail_mapping_save") })

	opts.EmbyBaseURL = "https://new-emby.example.com"
	if err := run(app, opts); err == nil {
		t.Fatal("run setup succeeded despite mapping save failure")
	}
	server, err = app.FindFirstRecordByData("emby_servers", "name", opts.EmbyServerName)
	if err != nil {
		t.Fatalf("find rolled back server: %v", err)
	}
	if server.GetString("base_url") != "https://emby.example.com" || server.GetString("server_id") != "upstream-server" || server.GetString("server_name") != "Upstream Emby" || server.GetString("server_version") != "4.9.0" || !server.GetDateTime("version_checked_at").Time().Equal(checkedAt) {
		t.Fatalf("server update was not rolled back: %#v", server)
	}
	for _, name := range []string{"backend", linkedAccount.GetString("name")} {
		account, err := app.FindFirstRecordByData("backend_accounts", "name", name)
		if err != nil {
			t.Fatalf("find rolled back account %q: %v", name, err)
		}
		assertBackendAccountAuthState(t, account, checkedAt, true)
	}
}

func TestSetupAllowsCustomBackendClientIdentity(t *testing.T) {
	app := newTestApp(t)
	opts := options{
		GatewayUsername:             "alice",
		GatewayPassword:             "gateway-pass",
		SyntheticUserID:             "gateway-alice",
		EmbyServerName:              "server",
		EmbyBaseURL:                 "https://emby.example.com",
		BackendAccountName:          "backend",
		BackendUsername:             "real-alice",
		BackendPassword:             "backend-pass",
		BackendUserAgent:            "Custom/1.0",
		BackendAuthorizationClient:  "Custom",
		BackendAuthorizationDevice:  "Desktop",
		BackendAuthorizationVersion: "1.0",
	}

	if err := run(app, opts); err != nil {
		t.Fatalf("run setup: %v", err)
	}

	server, err := app.FindFirstRecordByData("emby_servers", "name", "server")
	if err != nil {
		t.Fatalf("find server: %v", err)
	}
	if server.GetString("backend_user_agent") != "Custom/1.0" || server.GetString("backend_authorization_client") != "Custom" || server.GetString("backend_authorization_device") != "Desktop" || !uuidPattern.MatchString(server.GetString("backend_authorization_device_id")) || server.GetString("backend_authorization_version") != "1.0" {
		t.Fatalf("custom identity was not persisted: %#v", server)
	}
}

func newServer(t *testing.T, app core.App, name, baseURL string) *core.Record {
	t.Helper()
	collection, err := app.FindCollectionByNameOrId("emby_servers")
	if err != nil {
		t.Fatalf("find servers collection: %v", err)
	}
	server := core.NewRecord(collection)
	server.Set("name", name)
	server.Set("base_url", baseURL)
	server.Set("enabled", true)
	if err := app.Save(server); err != nil {
		t.Fatalf("save server: %v", err)
	}
	return server
}

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
	setBackendAccountAuthState(account, authenticatedAt)
	if err := app.Save(account); err != nil {
		t.Fatalf("save backend account: %v", err)
	}
	return account
}

func setBackendAccountAuthState(account *core.Record, authenticatedAt time.Time) {
	account.Set("backend_user_id", "upstream-user")
	account.Set("backend_token", "upstream-token")
	account.Set("token_updated_at", authenticatedAt)
	account.Set("last_login_at", authenticatedAt)
	account.Set("last_login_error", "previous failure")
}

func assertBackendAccountAuthState(t *testing.T, account *core.Record, authenticatedAt time.Time, wantPresent bool) {
	t.Helper()
	if wantPresent {
		if account.GetString("backend_user_id") != "upstream-user" || account.GetString("backend_token") != "upstream-token" || !account.GetDateTime("token_updated_at").Time().Equal(authenticatedAt) || !account.GetDateTime("last_login_at").Time().Equal(authenticatedAt) || account.GetString("last_login_error") != "previous failure" {
			t.Fatalf("account auth state was not preserved: %#v", account)
		}
		return
	}
	if account.GetString("backend_user_id") != "" || account.GetString("backend_token") != "" || !account.GetDateTime("token_updated_at").IsZero() || !account.GetDateTime("last_login_at").IsZero() || account.GetString("last_login_error") != "" {
		t.Fatalf("account auth state was not cleared: %#v", account)
	}
}

var uuidPattern = regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{4}-4[0-9A-F]{3}-[89AB][0-9A-F]{3}-[0-9A-F]{12}$`)

func newTestApp(t *testing.T) core.App {
	t.Helper()
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "test",
	})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	t.Cleanup(app.Cleanup)
	return app
}
