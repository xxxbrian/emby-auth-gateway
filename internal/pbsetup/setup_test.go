package pbsetup

import (
	"regexp"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
	_ "github.com/xxxbrian/emby-auth-gateway/internal/pbmigrations"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
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
	if account.GetString("backend_token") != "backend-token" {
		t.Fatalf("token was cleared without credential change")
	}

	opts.BackendPassword = "new-backend-pass"
	if err := run(app, opts); err != nil {
		t.Fatalf("run setup changed credentials: %v", err)
	}
	account, err = app.FindFirstRecordByData("backend_accounts", "name", "backend")
	if err != nil {
		t.Fatalf("find changed account: %v", err)
	}
	if account.GetString("backend_token") != "" || account.GetString("backend_user_id") != "" || !account.GetDateTime("token_updated_at").IsZero() {
		t.Fatalf("backend token state was not cleared: %#v", account)
	}
}

func TestSetupResetsServerIdentityOnlyWhenBaseURLChanges(t *testing.T) {
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
	account.Set("backend_user_id", "upstream-user")
	account.Set("backend_token", "preserved-token")
	account.Set("token_updated_at", checkedAt)
	if err := app.Save(account); err != nil {
		t.Fatalf("save backend account: %v", err)
	}

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
		account, err = app.FindFirstRecordByData("backend_accounts", "name", opts.BackendAccountName)
		if err != nil {
			t.Fatalf("find backend account: %v", err)
		}
		if account.GetString("backend_user_id") != "upstream-user" || account.GetString("backend_token") != "preserved-token" || !account.GetDateTime("token_updated_at").Time().Equal(checkedAt) {
			t.Fatalf("unrelated backend account fields changed: %#v", account)
		}
	}

	if err := run(app, opts); err != nil {
		t.Fatalf("run setup unchanged URL: %v", err)
	}
	assertServerIdentity(t, true)

	opts.EmbyBaseURL += "/"
	if err := run(app, opts); err != nil {
		t.Fatalf("run setup normalization-equivalent URL: %v", err)
	}
	assertServerIdentity(t, true)

	opts.EmbyBaseURL = "https://new-emby.example.com/emby"
	if err := run(app, opts); err != nil {
		t.Fatalf("run setup changed URL: %v", err)
	}
	assertServerIdentity(t, false)
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
