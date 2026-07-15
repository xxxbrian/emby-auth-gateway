package pbsetup

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func TestClassifyTokenOwnershipProtectsAllLegacyAndSingletonTokens(t *testing.T) {
	app, legacyServer, selected := laneBLegacy(t, "https://legacy.example.test")
	selected.Set("backend_token", "selected-token")
	if err := app.Save(selected); err != nil {
		t.Fatal(err)
	}
	other := newBackendAccount(t, app, "other", legacyServer.Id, "other", "password", true, selected.GetDateTime("last_login_at").Time())
	other.Set("backend_token", "other-token")
	if err := app.Save(other); err != nil {
		t.Fatal(err)
	}
	source, _ := laneBSingleton(t, app, legacyServer, selected, "https://legacy.example.test")
	source.Set("backend_token", "singleton-token")
	if err := app.Save(source); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		token string
		want  tokenOwnership
		err   bool
	}{{"singleton-token", tokenOwnershipProtected, true}, {"selected-token", tokenOwnershipProtected, true}, {"other-token", tokenOwnershipProtected, true}, {"", tokenOwnershipUnknown, false}, {"new-token", tokenOwnershipInvocation, false}} {
		got, err := classifyTokenOwnership(app, test.token)
		if got != test.want || (err != nil) != test.err {
			t.Fatalf("token %q: got (%v, %v), want (%v, error=%v)", test.token, got, err, test.want, test.err)
		}
	}
	readProtectedTokenAccounts = func(core.App) ([]*core.Record, error) { return nil, errors.New("read failed") }
	t.Cleanup(func() {
		readProtectedTokenAccounts = func(app core.App) ([]*core.Record, error) {
			return app.FindRecordsByFilter("backend_accounts", "", "", 0, 0, nil)
		}
	})
	if got, err := classifyTokenOwnership(app, "new-token"); got != tokenOwnershipUnknown || !isTokenOwnershipError(err) {
		t.Fatalf("ownership read failure got (%v, %v)", got, err)
	}
}

func TestImportDryRunProtectedTokenDoesNotLogoutWriteOrPrint(t *testing.T) {
	app := newTestApp(t)
	logouts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			_, _ = w.Write([]byte(`{"Id":"live-server"}`))
		case "/Users/AuthenticateByName":
			_, _ = w.Write([]byte(`{"AccessToken":"protected-token","ServerId":"live-server","User":{"Id":"backend-user"}}`))
		case "/Sessions/Logout":
			logouts++
		}
	}))
	defer server.Close()
	if err := run(app, options{GatewayUsername: "gateway", GatewayPassword: "password", SyntheticUserID: "synthetic", EmbyServerName: "selected", EmbyBaseURL: server.URL, BackendAccountName: "selected", BackendUsername: "backend", BackendPassword: "secret"}); err != nil {
		t.Fatal(err)
	}
	legacyServer, _ := app.FindFirstRecordByData("emby_servers", "name", "selected")
	account, _ := app.FindFirstRecordByData("backend_accounts", "name", "selected")
	account.Set("backend_token", "protected-token")
	if err := app.Save(account); err != nil {
		t.Fatal(err)
	}
	if _, data, err := runUpstreamImportLegacyPrepared(context.Background(), app, importLegacyOptions{ServerRecordID: legacyServer.Id, AccountRecordID: account.Id}, &bytes.Buffer{}); !isTokenOwnershipError(err) || data != nil {
		t.Fatalf("protected dry-run result data=%q err=%v", data, err)
	}
	if logouts != 0 {
		t.Fatalf("protected dry-run logout count=%d", logouts)
	}
	if count, _ := app.CountRecords(upstreamSources); count != 0 {
		t.Fatalf("protected dry-run wrote %d sources", count)
	}
}
