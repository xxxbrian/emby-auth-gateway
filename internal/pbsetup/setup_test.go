package pbsetup

import (
	"regexp"
	"testing"

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
