package pbsetup

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func TestLaneAImportActionMatrix(t *testing.T) {
	for _, test := range []struct {
		name, want string
		mutate     func(core.App, *importPlan)
	}{
		{name: "empty", want: "create"},
		{name: "exact complete", want: "noop", mutate: laneAAddExactSingleton},
		{name: "missing token", want: "repair-token", mutate: func(a core.App, p *importPlan) { laneAAddExactSingleton(a, p); p.state.source.Set("backend_token", "") }},
		{name: "missing backend user ID", want: "repair-token", mutate: func(a core.App, p *importPlan) {
			laneAAddExactSingleton(a, p)
			p.state.source.Set("backend_user_id", "")
		}},
		{name: "orphan endpoint", want: "conflict", mutate: func(a core.App, p *importPlan) {
			p.state.allEndpoints = []*core.Record{laneAEndpoint(a, "other", primaryEndpointKey, p.url, true)}
		}},
		{name: "no endpoint", want: "conflict", mutate: func(a core.App, p *importPlan) {
			laneAAddExactSingleton(a, p)
			p.state.endpoints, p.state.allEndpoints = nil, nil
		}},
		{name: "inactive endpoint", want: "conflict", mutate: func(a core.App, p *importPlan) {
			laneAAddExactSingleton(a, p)
			p.state.endpoints[0].Set("active", false)
		}},
		{name: "extra endpoint", want: "conflict", mutate: func(a core.App, p *importPlan) {
			laneAAddExactSingleton(a, p)
			p.state.endpoints = append(p.state.endpoints, laneAEndpoint(a, p.state.source.Id, "secondary", p.url+"/secondary", false))
			p.state.allEndpoints = append(p.state.allEndpoints, p.state.endpoints[1])
		}},
		{name: "endpoint relation elsewhere", want: "conflict", mutate: func(a core.App, p *importPlan) {
			laneAAddExactSingleton(a, p)
			p.state.endpoints[0].Set("source", "other")
			p.state.endpoints = nil
		}},
		{name: "source key mismatch", want: "conflict", mutate: func(a core.App, p *importPlan) { laneAAddExactSingleton(a, p); p.state.source.Set("key", "wrong") }},
		{name: "endpoint key mismatch", want: "conflict", mutate: func(a core.App, p *importPlan) {
			laneAAddExactSingleton(a, p)
			p.state.endpoints[0].Set("key", "wrong")
		}},
		{name: "wrong URL", want: "conflict", mutate: func(a core.App, p *importPlan) {
			laneAAddExactSingleton(a, p)
			p.state.endpoints[0].Set("base_url", p.url+"/wrong")
		}},
		{name: "source ServerId mismatch", want: "conflict", mutate: func(a core.App, p *importPlan) {
			laneAAddExactSingleton(a, p)
			p.state.source.Set("server_id", "wrong")
		}},
		{name: "source username mismatch", want: "conflict", mutate: func(a core.App, p *importPlan) {
			laneAAddExactSingleton(a, p)
			p.state.source.Set("backend_username", "wrong")
		}},
		{name: "source password mismatch", want: "conflict", mutate: func(a core.App, p *importPlan) {
			laneAAddExactSingleton(a, p)
			p.state.source.Set("backend_password", "wrong")
		}},
		{name: "source user agent mismatch", want: "conflict", mutate: func(a core.App, p *importPlan) {
			laneAAddExactSingleton(a, p)
			p.state.source.Set("backend_user_agent", "wrong")
		}},
		{name: "source client mismatch", want: "conflict", mutate: func(a core.App, p *importPlan) {
			laneAAddExactSingleton(a, p)
			p.state.source.Set("backend_authorization_client", "wrong")
		}},
		{name: "source device mismatch", want: "conflict", mutate: func(a core.App, p *importPlan) {
			laneAAddExactSingleton(a, p)
			p.state.source.Set("backend_authorization_device", "wrong")
		}},
		{name: "source version mismatch", want: "conflict", mutate: func(a core.App, p *importPlan) {
			laneAAddExactSingleton(a, p)
			p.state.source.Set("backend_authorization_version", "wrong")
		}},
		{name: "source DeviceId is managed", want: "noop", mutate: func(a core.App, p *importPlan) {
			laneAAddExactSingleton(a, p)
			p.state.source.Set("backend_authorization_device_id", "wrong")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, plan := laneAImportFixture(t)
			if test.mutate != nil {
				test.mutate(app, &plan)
			}
			if got := importAction(plan, upstreamProbeResult{serverID: "live-server", userID: "live-user", token: "live-token"}); got != test.want {
				t.Fatalf("importAction() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestLaneAImportConflictCleansValidationTokenWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(core.App, *core.Record, *core.Record)
	}{
		{name: "extra endpoint", mutate: func(app core.App, source, _ *core.Record) {
			laneASaveEndpoint(t, app, source.Id, "secondary", "https://extra.example.test", false)
		}},
		{name: "inactive endpoint", mutate: func(app core.App, _, endpoint *core.Record) {
			endpoint.Set("active", false)
			if err := app.Save(endpoint); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, plan := laneAImportFixture(t)
			auths, logouts := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/System/Info/Public":
					_, _ = w.Write([]byte(`{"Id":"live-server"}`))
				case "/Users/AuthenticateByName":
					auths++
					_, _ = w.Write([]byte(`{"AccessToken":"validation-token","ServerId":"live-server","User":{"Id":"live-user"}}`))
				case "/Sessions/Logout":
					logouts++
				}
			}))
			defer server.Close()
			plan.server.Set("base_url", server.URL)
			if err := app.Save(plan.server); err != nil {
				t.Fatal(err)
			}
			plan.url = server.URL
			source, endpoint := laneASaveExactSingleton(t, app, &plan)
			test.mutate(app, source, endpoint)
			before := laneAStateSnapshot(t, app)
			if _, err := runUpstreamImportLegacy(context.Background(), app, importLegacyOptions{ServerRecordID: plan.server.Id, AccountRecordID: plan.account.Id, Apply: true}, &bytes.Buffer{}); err == nil {
				t.Fatal("conflicting import succeeded")
			}
			if auths != 0 || logouts != 0 {
				t.Fatalf("auths=%d logouts=%d, want 0 for pre-auth conflict", auths, logouts)
			}
			after := laneAStateSnapshot(t, app)
			if strings.Join(after, "\n") != strings.Join(before, "\n") {
				t.Fatalf("conflicting import mutated state\nbefore=%q\nafter=%q", before, after)
			}
		})
	}
}

func TestLaneALoadImportPlanGuards(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(t *testing.T, app core.App, plan *importPlan)
		wantIDs bool
	}{
		{name: "disabled server", mutate: func(t *testing.T, app core.App, p *importPlan) {
			p.server.Set("enabled", false)
			if err := app.Save(p.server); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "disabled account", mutate: func(t *testing.T, app core.App, p *importPlan) {
			p.account.Set("enabled", false)
			if err := app.Save(p.account); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrong account server relation", mutate: func(t *testing.T, app core.App, p *importPlan) {
			other := newServer(t, app, "other", "https://other.example.test")
			p.account.Set("server", other.Id)
			if err := app.Save(p.account); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "enabled users mapped to wrong accounts", wantIDs: true, mutate: func(t *testing.T, app core.App, p *importPlan) {
			other := newBackendAccount(t, app, "other", p.server.Id, "other", "other", true, p.account.GetDateTime("last_login_at").Time())
			mapping, err := app.FindFirstRecordByData("user_mappings", "backend_account", p.account.Id)
			if err != nil {
				t.Fatal(err)
			}
			mapping.Set("backend_account", other.Id)
			if err := app.Save(mapping); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, plan := laneAImportFixture(t)
			test.mutate(t, app, &plan)
			_, err := loadImportPlan(app, importLegacyOptions{ServerRecordID: plan.server.Id, AccountRecordID: plan.account.Id})
			if err == nil {
				t.Fatal("guard accepted invalid legacy state")
			}
			if test.wantIDs {
				users, findErr := app.FindRecordsByFilter("users", "enabled = true", "id", 0, 0, nil)
				if findErr != nil {
					t.Fatal(findErr)
				}
				ids := make([]string, 0, len(users))
				for _, user := range users {
					ids = append(ids, user.Id)
				}
				sort.Strings(ids)
				if !strings.Contains(err.Error(), strings.Join(ids, ",")) {
					t.Fatalf("error %q does not contain sorted offenders %q", err, strings.Join(ids, ","))
				}
			}
			if sources, _ := app.CountRecords(upstreamSources); sources != 0 {
				t.Fatalf("guard wrote %d sources", sources)
			}
			if endpoints, _ := app.CountRecords(upstreamEndpoints); endpoints != 0 {
				t.Fatalf("guard wrote %d endpoints", endpoints)
			}
		})
	}
}

func laneAImportFixture(t *testing.T) (core.App, importPlan) {
	t.Helper()
	app := newTestApp(t)
	if err := run(app, options{GatewayUsername: "lanea", GatewayPassword: "gateway-password", SyntheticUserID: "lanea-user", EmbyServerName: "lanea-server", EmbyBaseURL: "https://legacy.example.test/emby", BackendAccountName: "lanea-account", BackendUsername: "legacy-user", BackendPassword: "legacy-password"}); err != nil {
		t.Fatal(err)
	}
	server, err := app.FindFirstRecordByData("emby_servers", "name", "lanea-server")
	if err != nil {
		t.Fatal(err)
	}
	account, err := app.FindFirstRecordByData("backend_accounts", "name", "lanea-account")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := loadImportPlan(app, importLegacyOptions{ServerRecordID: server.Id, AccountRecordID: account.Id})
	if err != nil {
		t.Fatal(err)
	}
	return app, plan
}

func laneAAddExactSingleton(app core.App, p *importPlan) {
	source, endpoint := laneAExactSingleton(app, p)
	p.state.source, p.state.endpoints, p.state.allEndpoints = source, []*core.Record{endpoint}, []*core.Record{endpoint}
}

func laneAExactSingleton(app core.App, p *importPlan) (*core.Record, *core.Record) {
	sources, err := app.FindCollectionByNameOrId(upstreamSources)
	if err != nil {
		panic(err)
	}
	source := core.NewRecord(sources)
	source.Id = "source"
	for field, value := range map[string]any{"key": defaultUpstreamKey, "server_id": "live-server", "backend_username": p.account.GetString("backend_username"), "backend_password": p.account.GetString("backend_password"), "backend_user_id": "live-user", "backend_token": "live-token", "auth_generation_id": "managed-generation", "backend_user_agent": p.identity.UserAgent, "backend_authorization_client": p.identity.Client, "backend_authorization_device": p.identity.Device, "backend_authorization_device_id": p.deviceID, "backend_authorization_version": p.identity.Version} {
		source.Set(field, value)
	}
	return source, laneAEndpoint(app, source.Id, primaryEndpointKey, p.url, true)
}

func laneAEndpoint(app core.App, sourceID, key, url string, active bool) *core.Record {
	collection, err := app.FindCollectionByNameOrId(upstreamEndpoints)
	if err != nil {
		panic(err)
	}
	record := core.NewRecord(collection)
	record.Set("source", sourceID)
	record.Set("key", key)
	record.Set("base_url", url)
	record.Set("active", active)
	return record
}

func laneASaveExactSingleton(t *testing.T, app core.App, p *importPlan) (*core.Record, *core.Record) {
	t.Helper()
	source, endpoint := laneAExactSingleton(app, p)
	source.Id = ""
	if err := app.Save(source); err != nil {
		t.Fatal(err)
	}
	endpoint.Set("source", source.Id)
	if err := app.Save(endpoint); err != nil {
		t.Fatal(err)
	}
	return source, endpoint
}

func laneASaveEndpoint(t *testing.T, app core.App, sourceID, key, url string, active bool) {
	t.Helper()
	if err := app.Save(laneAEndpoint(app, sourceID, key, url, active)); err != nil {
		t.Fatal(err)
	}
}

func laneAStateSnapshot(t *testing.T, app core.App) []string {
	t.Helper()
	names := []string{"users", "emby_servers", "backend_accounts", "user_mappings", upstreamSources, upstreamEndpoints}
	var snapshot []string
	for _, name := range names {
		records, err := app.FindRecordsByFilter(name, "", "id", 0, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range records {
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			snapshot = append(snapshot, name+":"+string(data))
		}
	}
	sort.Strings(snapshot)
	return snapshot
}
