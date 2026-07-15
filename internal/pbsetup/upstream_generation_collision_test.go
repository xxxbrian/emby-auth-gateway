package pbsetup

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func TestGenCollisionPartialAuthProtectedTokensDoNotLogoutOrWrite(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "wrong server type", body: `{"AccessToken":"%s","ServerId":1,"User":{"Id":"new-user"}}`},
		{name: "missing user", body: `{"AccessToken":"%s","ServerId":"live-server"}`},
		{name: "wrong user type", body: `{"AccessToken":"%s","ServerId":"live-server","User":"bad"}`},
	} {
		for _, token := range []string{"current-token", "selected-token", "other-token"} {
			t.Run(test.name+"/"+token, func(t *testing.T) {
				app, legacyServer, account, source := genCollisionProtectedFixture(t, "")
				logouts := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/System/Info/Public":
						_, _ = w.Write([]byte(`{"Id":"live-server"}`))
					case "/Users/AuthenticateByName":
						_, _ = w.Write([]byte(strings.ReplaceAll(test.body, "%s", token)))
					case "/Sessions/Logout":
						logouts++
					}
				}))
				defer server.Close()
				genCollisionSetURL(t, app, legacyServer, source, server.URL)
				before := genCollisionAuthTuple(source)

				if err := runUpstreamCreate(context.Background(), app, upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "changed", BackendPassword: account.GetString("backend_password")}); err == nil {
					t.Fatal("Phase2A update accepted partial authentication")
				}
				source, _ = app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
				if logouts != 0 || genCollisionAuthTuple(source) != before {
					t.Fatalf("Phase2A logout=%d tuple=%q want no logout and %q", logouts, genCollisionAuthTuple(source), before)
				}
				source.Set("backend_user_id", "")
				if err := app.Save(source); err != nil {
					t.Fatal(err)
				}
				before = genCollisionAuthTuple(source)

				var stdout bytes.Buffer
				if _, data, err := runUpstreamImportLegacyPrepared(context.Background(), app, importLegacyOptions{ServerRecordID: legacyServer.Id, AccountRecordID: account.Id, Apply: true}, &stdout); err == nil || data != nil {
					t.Fatalf("import err=%v success output=%q", err, data)
				}
				source, _ = app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
				if logouts != 0 || genCollisionAuthTuple(source) != before || stdout.Len() != 0 {
					t.Fatalf("import logout=%d tuple=%q stderr=%q", logouts, genCollisionAuthTuple(source), stdout.String())
				}
			})
		}
	}
}

func TestGenCollisionOwnershipLoaderFailureFailsClosedThroughCallers(t *testing.T) {
	for _, operation := range []string{"Phase2A", "import"} {
		t.Run(operation, func(t *testing.T) {
			app, legacyServer, account, source := genCollisionProtectedFixture(t, "")
			logouts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/System/Info/Public":
					_, _ = w.Write([]byte(`{"Id":"live-server"}`))
				case "/Users/AuthenticateByName":
					_, _ = w.Write([]byte(`{"AccessToken":"invocation-token","ServerId":"live-server","User":{"Id":"new-user"}}`))
				case "/Sessions/Logout":
					logouts++
				}
			}))
			defer server.Close()
			genCollisionSetURL(t, app, legacyServer, source, server.URL)
			before := genCollisionAuthTuple(source)
			readProtectedTokenAccounts = func(core.App) ([]*core.Record, error) { return nil, errors.New("ownership query failed") }
			t.Cleanup(func() {
				readProtectedTokenAccounts = func(app core.App) ([]*core.Record, error) {
					return app.FindRecordsByFilter("backend_accounts", "", "", 0, 0, nil)
				}
			})

			var err error
			if operation == "Phase2A" {
				err = runUpstreamCreate(context.Background(), app, upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "changed", BackendPassword: account.GetString("backend_password")})
			} else {
				source.Set("backend_user_id", "")
				if saveErr := app.Save(source); saveErr != nil {
					t.Fatal(saveErr)
				}
				before = genCollisionAuthTuple(source)
				_, _, err = runUpstreamImportLegacyPrepared(context.Background(), app, importLegacyOptions{ServerRecordID: legacyServer.Id, AccountRecordID: account.Id, Apply: true}, &bytes.Buffer{})
			}
			if !isTokenOwnershipError(err) {
				t.Fatalf("error=%v, want ownership failure", err)
			}
			source, _ = app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
			if logouts != 0 || genCollisionAuthTuple(source) != before {
				t.Fatalf("logout=%d tuple=%q want no logout and %q", logouts, genCollisionAuthTuple(source), before)
			}
		})
	}
}

func TestGenCollisionGenerationRotatesOnUpdatesAndStaysOnNoop(t *testing.T) {
	t.Run("Phase2A update and noop", func(t *testing.T) {
		app, _, opts, closeServer := establishedUpstream(t)
		defer closeServer()
		source, _ := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
		oldGeneration := source.GetString("auth_generation_id")
		opts.BackendUsername, upstreamTestToken = "changed", "new-token"
		if err := runUpstreamCreate(context.Background(), app, opts); err != nil {
			t.Fatal(err)
		}
		source, _ = app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
		genCollisionAssertRotatedTuple(t, source, oldGeneration, "new-token", "user")
		generation := source.GetString("auth_generation_id")
		if err := runUpstreamCreate(context.Background(), app, opts); err != nil {
			t.Fatal(err)
		}
		source, _ = app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
		if source.GetString("auth_generation_id") != generation {
			t.Fatalf("no-op generation changed from %q to %q", generation, source.GetString("auth_generation_id"))
		}
	})

	t.Run("import repair", func(t *testing.T) {
		server := laneBServer(t, func(string) {})
		defer server.Close()
		app, legacyServer, account := laneBLegacy(t, server.URL)
		source, _ := laneBSingleton(t, app, legacyServer, account, server.URL)
		oldGeneration := source.GetString("auth_generation_id")
		source.Set("backend_user_id", "")
		if err := app.Save(source); err != nil {
			t.Fatal(err)
		}
		if _, err := runUpstreamImportLegacy(context.Background(), app, importLegacyOptions{ServerRecordID: legacyServer.Id, AccountRecordID: account.Id, Apply: true}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		source, _ = app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
		genCollisionAssertRotatedTuple(t, source, oldGeneration, "new-auth-token", "new-user")
	})
}

func TestGenCollisionGenerationOnlyDriftCleansOnlyInvocationToken(t *testing.T) {
	app, _, opts, closeServer := establishedUpstream(t)
	defer closeServer()
	opts.BackendUsername, upstreamTestToken = "changed", "new-token"
	source, _ := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
	logouts := []string{}
	upstreamTestLogout = func(token string) { logouts = append(logouts, token) }
	afterUpstreamProbe = func() {
		record, _ := app.FindRecordById(upstreamSources, source.Id)
		record.Set("auth_generation_id", "concurrent-generation")
		_ = app.Save(record)
	}
	t.Cleanup(func() { afterUpstreamProbe = nil; upstreamTestLogout = nil; upstreamTestToken = "" })
	if err := runUpstreamCreate(context.Background(), app, opts); err == nil {
		t.Fatal("generation-only drift was accepted")
	}
	source, _ = app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
	if source.GetString("backend_token") != "old-token" || source.GetString("auth_generation_id") != "concurrent-generation" || strings.Join(logouts, ",") != "new-token" {
		t.Fatalf("source token=%q generation=%q logouts=%q", source.GetString("backend_token"), source.GetString("auth_generation_id"), logouts)
	}
}

func genCollisionProtectedFixture(t *testing.T, url string) (core.App, *core.Record, *core.Record, *core.Record) {
	t.Helper()
	if url == "" {
		url = "https://legacy.example.test"
	}
	app, server, account := laneBLegacy(t, url)
	account.Set("backend_token", "selected-token")
	if err := app.Save(account); err != nil {
		t.Fatal(err)
	}
	other := newBackendAccount(t, app, "other", server.Id, "other", "password", true, account.GetDateTime("last_login_at").Time())
	other.Set("backend_token", "other-token")
	if err := app.Save(other); err != nil {
		t.Fatal(err)
	}
	source, _ := laneBSingleton(t, app, server, account, url)
	source.Set("backend_token", "current-token")
	if err := app.Save(source); err != nil {
		t.Fatal(err)
	}
	return app, server, account, source
}

func genCollisionSetURL(t *testing.T, app core.App, server, source *core.Record, url string) {
	t.Helper()
	server.Set("base_url", url)
	if err := app.Save(server); err != nil {
		t.Fatal(err)
	}
	endpoint, err := app.FindFirstRecordByData(upstreamEndpoints, "source", source.Id)
	if err != nil {
		t.Fatal(err)
	}
	endpoint.Set("base_url", url)
	if err := app.Save(endpoint); err != nil {
		t.Fatal(err)
	}
}

func genCollisionAuthTuple(source *core.Record) string {
	return strings.Join([]string{source.GetString("backend_token"), source.GetString("backend_user_id"), source.GetString("backend_authorization_device_id"), source.GetString("auth_generation_id")}, "|")
}

func genCollisionAssertRotatedTuple(t *testing.T, source *core.Record, oldGeneration, token, user string) {
	t.Helper()
	if source.GetString("backend_token") != token || source.GetString("backend_user_id") != user || source.GetString("backend_authorization_device_id") == "" || source.GetString("auth_generation_id") == "" || source.GetString("auth_generation_id") == oldGeneration {
		t.Fatalf("auth tuple was not atomically refreshed: %q", genCollisionAuthTuple(source))
	}
}
