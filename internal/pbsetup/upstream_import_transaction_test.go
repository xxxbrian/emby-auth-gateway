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
	"github.com/pocketbase/pocketbase/tools/hook"
)

func TestLaneBImportRepairTokenCommitsBeforeOldSingletonCleanup(t *testing.T) {
	for _, missing := range []string{"token", "user ID"} {
		cleanups := []string{"success"}
		if missing == "user ID" {
			cleanups = []string{"success", "status", "transport"}
		}
		for _, cleanup := range cleanups {
			t.Run(missing+"/"+cleanup, func(t *testing.T) {
				var logouts []string
				var app core.App
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/System/Info/Public":
						_, _ = w.Write([]byte(`{"Id":"live-server","ServerName":"live","Version":"4.9"}`))
					case "/Users/AuthenticateByName":
						_, _ = w.Write([]byte(`{"AccessToken":"new-auth-token","ServerId":"live-server","User":{"Id":"new-user"}}`))
					case "/Sessions/Logout":
						token := laneBToken(r)
						logouts = append(logouts, token)
						if token == "old-singleton-token" {
							source, _ := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
							if source.GetString("backend_token") != "new-auth-token" {
								t.Error("old token cleanup began before the replacement committed")
							}
							switch cleanup {
							case "status":
								w.WriteHeader(http.StatusBadGateway)
							case "transport":
								conn, _, err := w.(http.Hijacker).Hijack()
								if err == nil {
									_ = conn.Close()
								}
							}
						}
					}
				}))
				defer server.Close()
				app, legacyServer, account := laneBLegacy(t, server.URL)
				source, endpoint := laneBSingleton(t, app, legacyServer, account, server.URL)
				if missing == "token" {
					source.Set("backend_token", "")
				} else {
					source.Set("backend_user_id", "")
				}
				if err := app.Save(source); err != nil {
					t.Fatal(err)
				}
				source, _ = app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
				endpoint, _ = app.FindFirstRecordByData(upstreamEndpoints, "source", source.Id)
				before := laneBRepairSnapshot(source, endpoint)
				var stderr bytes.Buffer
				summary, err := runUpstreamImportLegacy(context.Background(), app, importLegacyOptions{ServerRecordID: legacyServer.Id, AccountRecordID: account.Id, Apply: true}, &stderr)
				if err != nil || summary.Action != "repair-token" || summary.ValidationToken != "persisted" {
					t.Fatalf("repair summary=%+v err=%v", summary, err)
				}
				source, _ = app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
				endpoint, _ = app.FindFirstRecordByData(upstreamEndpoints, "source", source.Id)
				laneBAssertRepairImmutable(t, before, source, endpoint)
				if source.GetString("backend_token") != "new-auth-token" || source.GetString("backend_user_id") != "new-user" || source.GetString("server_name") != "live" || source.GetString("server_version") != "4.9" || source.GetString("last_login_error") != "" || source.GetDateTime("token_updated_at").IsZero() || source.GetDateTime("last_login_at").IsZero() || source.GetDateTime("version_checked_at").IsZero() {
					t.Fatal("repair did not limit changes to refreshed authentication/login/live metadata")
				}
				wantLogouts := 1
				if missing == "token" {
					wantLogouts = 0
				}
				if account.GetString("backend_token") != "legacy-account-token" || strings.Contains(strings.Join(logouts, ","), "legacy-account-token") || strings.Contains(strings.Join(logouts, ","), "new-auth-token") || len(logouts) != wantLogouts || wantLogouts == 1 && logouts[0] != "old-singleton-token" {
					t.Fatalf("unexpected token handling: legacy=%q logouts=%q", account.GetString("backend_token"), logouts)
				}
				warning := stderr.String()
				if cleanup == "success" && warning != "" {
					t.Fatalf("unexpected cleanup warning: %q", warning)
				}
				if cleanup != "success" && (!strings.Contains(warning, "warning: old singleton token cleanup failed") || strings.Contains(warning, "old-singleton-token") || strings.Contains(warning, "new-auth-token") || strings.Contains(warning, "legacy-account-token")) {
					t.Fatalf("cleanup warning was missing or leaked a secret: %q", warning)
				}
			})
		}
	}
}

func TestLaneBImportRejectsDriftAndCleansNewToken(t *testing.T) {
	for _, drift := range []string{"legacy server", "legacy account", "enabled user", "mapping", "source", "generation", "endpoint"} {
		t.Run(drift, func(t *testing.T) {
			var mutate func()
			logouts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/System/Info/Public":
					_, _ = w.Write([]byte(`{"Id":"live-server"}`))
				case "/Users/AuthenticateByName":
					_, _ = w.Write([]byte(`{"AccessToken":"new-auth-token","ServerId":"live-server","User":{"Id":"new-user"}}`))
					mutate()
				case "/Sessions/Logout":
					if laneBToken(r) == "new-auth-token" {
						logouts++
					}
				}
			}))
			defer server.Close()
			app, legacyServer, account := laneBLegacy(t, server.URL)
			source, endpoint := laneBSingleton(t, app, legacyServer, account, server.URL)
			source.Set("backend_user_id", "")
			if err := app.Save(source); err != nil {
				t.Fatal(err)
			}
			user, _ := app.FindFirstRecordByData("users", "username", "gateway")
			mapping, _ := app.FindFirstRecordByData("user_mappings", "gateway_user", user.Id)
			mutate = func() {
				switch drift {
				case "legacy server":
					record, _ := app.FindRecordById("emby_servers", legacyServer.Id)
					record.Set("base_url", server.URL+"/changed")
					_ = app.Save(record)
				case "legacy account":
					record, _ := app.FindRecordById("backend_accounts", account.Id)
					record.Set("backend_password", "concurrent")
					_ = app.Save(record)
				case "enabled user":
					record, _ := app.FindRecordById("users", user.Id)
					record.Set("enabled", false)
					_ = app.Save(record)
				case "mapping":
					record, _ := app.FindRecordById("user_mappings", mapping.Id)
					record.Set("enabled", false)
					_ = app.Save(record)
				case "source":
					record, _ := app.FindRecordById(upstreamSources, source.Id)
					record.Set("last_login_error", "concurrent")
					_ = app.Save(record)
				case "generation":
					record, _ := app.FindRecordById(upstreamSources, source.Id)
					record.Set("auth_generation_id", "concurrent")
					_ = app.Save(record)
				case "endpoint":
					record, _ := app.FindRecordById(upstreamEndpoints, endpoint.Id)
					record.Set("key", "concurrent")
					_ = app.Save(record)
				}
			}
			if _, err := runUpstreamImportLegacy(context.Background(), app, importLegacyOptions{ServerRecordID: legacyServer.Id, AccountRecordID: account.Id, Apply: true}, &bytes.Buffer{}); err == nil {
				t.Fatal("concurrent drift was accepted")
			}
			source, _ = app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
			endpoint, _ = app.FindFirstRecordByData(upstreamEndpoints, "source", source.Id)
			legacyServer, _ = app.FindRecordById("emby_servers", legacyServer.Id)
			account, _ = app.FindRecordById("backend_accounts", account.Id)
			user, _ = app.FindRecordById("users", user.Id)
			mapping, _ = app.FindRecordById("user_mappings", mapping.Id)
			if source.GetString("backend_token") != "old-singleton-token" || logouts != 1 {
				t.Fatalf("drift overwrote singleton or failed cleanup: token=%q cleanup=%d", source.GetString("backend_token"), logouts)
			}
			if drift == "legacy server" && legacyServer.GetString("base_url") != server.URL+"/changed" || drift == "source" && source.GetString("last_login_error") != "concurrent" || drift == "generation" && source.GetString("auth_generation_id") != "concurrent" || drift == "endpoint" && endpoint.GetString("key") != "concurrent" || drift == "legacy account" && account.GetString("backend_password") != "concurrent" || drift == "enabled user" && user.GetBool("enabled") || drift == "mapping" && mapping.GetBool("enabled") {
				t.Fatal("concurrent state was not preserved exactly")
			}
		})
	}
}

func TestLaneBImportRollbackAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name, action string
		repair       bool
	}{{"endpoint save failure", "endpoint-failure", false}, {"cancellation after reread", "reread-cancel", true}, {"cancellation after source save", "source-cancel", false}} {
		t.Run(test.name, func(t *testing.T) {
			logouts := 0
			server := laneBServer(t, func(token string) {
				if token == "new-auth-token" {
					logouts++
				}
			})
			defer server.Close()
			app, legacyServer, account := laneBLegacy(t, server.URL)
			if test.repair {
				source, _ := laneBSingleton(t, app, legacyServer, account, server.URL)
				source.Set("backend_user_id", "")
				if err := app.Save(source); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.action == "reread-cancel" {
				afterImportPlanReread = cancel
				t.Cleanup(func() { afterImportPlanReread = nil })
			}
			if test.action == "source-cancel" {
				afterImportSourceSave = cancel
				t.Cleanup(func() { afterImportSourceSave = nil })
			}
			if test.action == "endpoint-failure" {
				collection, _ := app.FindCollectionByNameOrId(upstreamEndpoints)
				app.OnRecordCreateExecute(collection.Id).Bind(&hook.Handler[*core.RecordEvent]{Id: "laneB_endpoint_failure", Priority: 999999, Func: func(*core.RecordEvent) error { return errors.New("endpoint save failed") }})
				t.Cleanup(func() { app.OnRecordCreateExecute(collection.Id).Unbind("laneB_endpoint_failure") })
			}
			if _, err := runUpstreamImportLegacy(ctx, app, importLegacyOptions{ServerRecordID: legacyServer.Id, AccountRecordID: account.Id, Apply: true}, &bytes.Buffer{}); err == nil {
				t.Fatal("failed transaction succeeded")
			}
			if test.repair {
				source, _ := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
				if source.GetString("backend_token") != "old-singleton-token" || source.GetString("backend_user_id") != "" {
					t.Fatal("cancelled repair changed singleton state")
				}
			} else if sources, _ := app.CountRecords(upstreamSources); sources != 0 {
				t.Fatalf("partial source persisted: %d", sources)
			}
			if endpoints, _ := app.CountRecords(upstreamEndpoints); test.repair && endpoints != 1 || !test.repair && endpoints != 0 {
				t.Fatalf("unexpected endpoint count after rollback: %d", endpoints)
			}
			if logouts != 1 {
				t.Fatalf("detached new-token cleanup=%d", logouts)
			}
		})
	}
}

type laneBImmutableRepairState struct {
	sourceID, sourceKey, sourceServerID, sourceUsername, sourcePassword string
	sourceUserAgent, sourceClient, sourceDevice, sourceDeviceID         string
	sourceVersion, sourceCreated                                        string
	endpointID, endpointSource, endpointKey, endpointURL                string
	endpointCreated, endpointUpdated                                    string
	endpointActive                                                      bool
}

func laneBRepairSnapshot(source, endpoint *core.Record) laneBImmutableRepairState {
	return laneBImmutableRepairState{
		sourceID: source.Id, sourceKey: source.GetString("key"), sourceServerID: source.GetString("server_id"), sourceUsername: source.GetString("backend_username"), sourcePassword: source.GetString("backend_password"),
		sourceUserAgent: source.GetString("backend_user_agent"), sourceClient: source.GetString("backend_authorization_client"), sourceDevice: source.GetString("backend_authorization_device"), sourceDeviceID: source.GetString("backend_authorization_device_id"),
		sourceVersion: source.GetString("backend_authorization_version"), sourceCreated: source.GetDateTime("created").String(),
		endpointID: endpoint.Id, endpointSource: endpoint.GetString("source"), endpointKey: endpoint.GetString("key"), endpointURL: endpoint.GetString("base_url"), endpointActive: endpoint.GetBool("active"), endpointCreated: endpoint.GetDateTime("created").String(), endpointUpdated: endpoint.GetDateTime("updated").String(),
	}
}

func laneBAssertRepairImmutable(t *testing.T, before laneBImmutableRepairState, source, endpoint *core.Record) {
	t.Helper()
	after := laneBRepairSnapshot(source, endpoint)
	if after.sourceDeviceID == before.sourceDeviceID {
		t.Fatal("repair did not rotate the managed DeviceId")
	}
	after.sourceDeviceID = before.sourceDeviceID
	if after != before {
		t.Fatalf("repair changed immutable singleton configuration:\n before=%+v\n after=%+v", before, after)
	}
}

func laneBLegacy(t *testing.T, url string) (core.App, *core.Record, *core.Record) {
	t.Helper()
	app := newTestApp(t)
	server, account := seedLegacyImportRecords(t, app, "selected", url, "selected", "backend", "legacy-password")
	server.Set("server_id", "live-server")
	account.Set("backend_token", "legacy-account-token")
	if err := app.Save(server); err != nil {
		t.Fatal(err)
	}
	if err := app.Save(account); err != nil {
		t.Fatal(err)
	}
	return app, server, account
}

func laneBSingleton(t *testing.T, app core.App, server, account *core.Record, url string) (*core.Record, *core.Record) {
	t.Helper()
	sources, _ := app.FindCollectionByNameOrId(upstreamSources)
	source := core.NewRecord(sources)
	for key, value := range map[string]any{"key": defaultUpstreamKey, "server_id": "live-server", "server_name": "old-live", "server_version": "4.8", "backend_username": account.GetString("backend_username"), "backend_password": account.GetString("backend_password"), "backend_user_id": "old-user", "backend_token": "old-singleton-token", "auth_generation_id": "managed-generation", "last_login_error": "old error", "backend_user_agent": server.GetString("backend_user_agent"), "backend_authorization_client": server.GetString("backend_authorization_client"), "backend_authorization_device": server.GetString("backend_authorization_device"), "backend_authorization_device_id": server.GetString("backend_authorization_device_id"), "backend_authorization_version": server.GetString("backend_authorization_version")} {
		source.Set(key, value)
	}
	if err := app.Save(source); err != nil {
		t.Fatal(err)
	}
	endpoints, _ := app.FindCollectionByNameOrId(upstreamEndpoints)
	endpoint := core.NewRecord(endpoints)
	endpoint.Set("source", source.Id)
	endpoint.Set("key", primaryEndpointKey)
	endpoint.Set("base_url", url)
	endpoint.Set("active", true)
	if err := app.Save(endpoint); err != nil {
		t.Fatal(err)
	}
	return source, endpoint
}

func laneBServer(t *testing.T, logout func(string)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			_, _ = w.Write([]byte(`{"Id":"live-server"}`))
		case "/Users/AuthenticateByName":
			_, _ = w.Write([]byte(`{"AccessToken":"new-auth-token","ServerId":"live-server","User":{"Id":"new-user"}}`))
		case "/Sessions/Logout":
			logout(laneBToken(r))
		}
	}))
}

func laneBToken(r *http.Request) string {
	for _, token := range []string{"old-singleton-token", "legacy-account-token", "new-auth-token"} {
		if strings.Contains(r.Header.Get("X-Emby-Authorization"), `Token="`+token+`"`) {
			return token
		}
	}
	return ""
}
