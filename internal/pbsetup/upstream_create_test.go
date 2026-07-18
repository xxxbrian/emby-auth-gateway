package pbsetup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xxxbrian/emby-auth-gateway/internal/controlplane"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
)

func TestSingletonSetupCommandSuccessOutput(t *testing.T) {
	app := newTestApp(t)
	backend := newUpstreamResponder(t)
	defer backend.Close()

	upstream := NewCommand(app)
	var upstreamOutput bytes.Buffer
	upstream.SetOut(&upstreamOutput)
	upstream.SetArgs([]string{"upstream", "create", "--emby-url", backend.URL, "--backend-username", "backend", "--backend-password", "password"})
	if err := upstream.Execute(); err != nil {
		t.Fatalf("upstream create: %v", err)
	}
	if got := upstreamOutput.String(); got != "configured singleton upstream\n" {
		t.Fatalf("upstream output = %q", got)
	}

	user := NewCommand(app)
	var userOutput bytes.Buffer
	user.SetOut(&userOutput)
	user.SetArgs([]string{"user", "--gateway-username", "alice", "--gateway-password", "password", "--synthetic-user-id", "gateway-alice"})
	if err := user.Execute(); err != nil {
		t.Fatalf("setup user: %v", err)
	}
	if got := userOutput.String(); got != "configured gateway user \"alice\"\n" {
		t.Fatalf("user output = %q", got)
	}
}

var (
	upstreamTestToken      string
	upstreamTestLogout     func(string)
	upstreamTestLogoutMode string
)

func newUpstreamResponder(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			_, _ = w.Write([]byte(`{"Id":"server"}`))
		case "/Users/AuthenticateByName":
			token := upstreamTestToken
			if token == "" {
				token = "old-token"
			}
			_, _ = w.Write([]byte(`{"AccessToken":"` + token + `","ServerId":"server","User":{"Id":"user"}}`))
		case "/Sessions/Logout":
			if upstreamTestLogout != nil {
				token := ""
				if strings.Contains(r.Header.Get("X-Emby-Authorization"), `Token="new-token"`) {
					token = "new-token"
				}
				if strings.Contains(r.Header.Get("X-Emby-Authorization"), `Token="old-token"`) {
					token = "old-token"
				}
				upstreamTestLogout(token)
			}
			switch upstreamTestLogoutMode {
			case "redirect":
				http.Redirect(w, r, "/elsewhere", http.StatusFound)
			case "status":
				w.WriteHeader(http.StatusBadGateway)
			}
		}
	}))
}

func establishedUpstream(t *testing.T) (core.App, *httptest.Server, upstreamOptions, func()) {
	t.Helper()
	app := newTestApp(t)
	server := newUpstreamResponder(t)
	upstreamTestToken = "old-token"
	if err := runUpstreamCreate(context.Background(), app, upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "u", BackendPassword: "p"}); err != nil {
		t.Fatal(err)
	}
	return app, server, upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "u", BackendPassword: "p"}, func() { server.Close(); upstreamTestToken = ""; upstreamTestLogout = nil; upstreamTestLogoutMode = "" }
}

func TestSetupCommandContainsOnlyCurrentSingletonChildren(t *testing.T) {
	app := newTestApp(t)
	setup := NewCommand(app)
	if setup.RunE == nil || setup.Args == nil {
		t.Fatal("legacy setup command must reject positional arguments")
	}
	upstreamGroup, _, err := setup.Find([]string{"upstream"})
	if err != nil || upstreamGroup.Name() != "upstream" || upstreamGroup.RunE == nil || upstreamGroup.Args == nil {
		t.Fatalf("missing safe upstream command group: %v", err)
	}
	upstream, _, err := setup.Find([]string{"upstream", "create"})
	if err != nil || upstream.Name() != "create" || upstream.Args == nil {
		t.Fatalf("missing upstream create cobra.NoArgs command: %v", err)
	}
	if children := upstreamGroup.Commands(); len(children) != 1 || children[0].Name() != "create" {
		t.Fatalf("upstream commands = %#v, want only create", children)
	}
	user, _, err := setup.Find([]string{"user"})
	if err != nil || user.Args == nil {
		t.Fatalf("missing user cobra.NoArgs command: %v", err)
	}
}

func TestUpstreamCreateProbesAndPersistsThenNoops(t *testing.T) {
	app := newTestApp(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("User-Agent") == "" || !strings.Contains(r.Header.Get("X-Emby-Authorization"), "DeviceId=") {
			t.Errorf("missing identity headers: %#v", r.Header)
		}
		switch r.URL.Path {
		case "/emby/System/Info/Public":
			if r.Method != http.MethodGet || strings.Contains(r.Header.Get("X-Emby-Authorization"), "Token=") {
				t.Errorf("public request was not tokenless")
			}
			_, _ = w.Write([]byte(`{"Id":"server-id","ServerName":"Emby","Version":"4.9"}`))
		case "/emby/Users/AuthenticateByName":
			if r.Method != http.MethodPost {
				t.Errorf("auth method = %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"AccessToken":"token","ServerId":"server-id","User":{"Id":"backend-user"}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	opts := upstreamOptions{EmbyBaseURL: server.URL + "/emby/", BackendUsername: "backend", BackendPassword: "secret"}
	if err := runUpstreamCreate(context.Background(), app, opts); err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	source, err := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
	if err != nil {
		t.Fatal(err)
	}
	if source.GetString("backend_token") != "token" || source.GetString("backend_user_id") != "backend-user" || source.GetString("backend_authorization_device_id") == "" {
		t.Fatalf("source was not fully persisted: %#v", source)
	}
	endpoint, err := app.FindFirstRecordByData(upstreamEndpoints, "source", source.Id)
	if err != nil || endpoint.GetString("base_url") != server.URL+"/emby" || endpoint.GetString("key") != primaryEndpointKey || !endpoint.GetBool("active") {
		t.Fatalf("endpoint persistence: %#v, %v", endpoint, err)
	}
	deviceID := source.GetString("backend_authorization_device_id")
	if err := runUpstreamCreate(context.Background(), app, opts); err != nil {
		t.Fatalf("noop upstream: %v", err)
	}
	if requests != 3 {
		t.Fatalf("no-op must make only the public namespace probe: %d", requests)
	}
	source, _ = app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
	if source.GetString("backend_authorization_device_id") != deviceID {
		t.Fatal("device ID changed on no-op")
	}
}

func TestUpstreamCreateRejectsPublicIDBeforeAuthentication(t *testing.T) {
	app := newTestApp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Users/AuthenticateByName" {
			t.Fatal("authentication ran after public server ID mismatch")
		}
		_, _ = w.Write([]byte(`{"Id":"new-server"}`))
	}))
	defer server.Close()
	sources, _ := app.FindCollectionByNameOrId(upstreamSources)
	source := core.NewRecord(sources)
	for key, value := range map[string]any{"key": defaultUpstreamKey, "server_id": "old-server", "backend_username": "u", "backend_password": "p", "backend_user_agent": "ua", "backend_authorization_client": "c", "backend_authorization_device": "d", "backend_authorization_device_id": "device", "backend_authorization_version": "v"} {
		source.Set(key, value)
	}
	if err := app.Save(source); err != nil {
		t.Fatal(err)
	}
	endpoints, _ := app.FindCollectionByNameOrId(upstreamEndpoints)
	endpoint := core.NewRecord(endpoints)
	endpoint.Set("source", source.Id)
	endpoint.Set("key", primaryEndpointKey)
	endpoint.Set("base_url", server.URL)
	endpoint.Set("active", true)
	if err := app.Save(endpoint); err != nil {
		t.Fatal(err)
	}
	if err := runUpstreamCreate(context.Background(), app, upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "u", BackendPassword: "p", BackendUserAgent: "ua", BackendAuthorizationClient: "c", BackendAuthorizationDevice: "d", BackendAuthorizationVersion: "v"}); err == nil {
		t.Fatal("server replacement was accepted")
	}
}

func TestNormalizeUpstreamURL(t *testing.T) {
	for _, test := range []struct {
		raw, want string
		valid     bool
	}{
		{"https://emby.example.com/emby///", "https://emby.example.com/emby", true},
		{"http://emby.example.com", "http://emby.example.com", true},
		{"/relative", "", false}, {"ftp://emby.example.com", "", false}, {"https://u:p@emby.example.com", "", false}, {"https://emby.example.com/?x=1", "", false}, {"https://emby.example.com/#x", "", false}, {"https://", "", false},
	} {
		got, err := normalizeUpstreamURL(test.raw)
		if test.valid != (err == nil) || got != test.want {
			t.Errorf("normalizeUpstreamURL(%q) = %q, %v", test.raw, got, err)
		}
	}
}

func TestUpstreamCreateLogsOutPartialAuthenticationTokens(t *testing.T) {
	for name, auth := range map[string]string{
		"missing server":     `{"AccessToken":"new-token","User":{"Id":"user"}}`,
		"whitespace server":  `{"AccessToken":"new-token","ServerId":" ","User":{"Id":"user"}}`,
		"missing user":       `{"AccessToken":"new-token","ServerId":"server"}`,
		"whitespace user":    `{"AccessToken":"new-token","ServerId":"server","User":{"Id":" "}}`,
		"server mismatch":    `{"AccessToken":"new-token","ServerId":"other","User":{"Id":"user"}}`,
		"whitespace token":   `{"AccessToken":" new-token ","ServerId":"server","User":{"Id":"user"}}`,
		"wrong type server":  `{"AccessToken":"new-token","ServerId":1,"User":{"Id":"user"}}`,
		"wrong type user id": `{"AccessToken":"new-token","ServerId":"server","User":{"Id":1}}`,
		"malformed user":     `{"AccessToken":"new-token","ServerId":"server","User":"bad"}`,
	} {
		t.Run(name, func(t *testing.T) {
			logout := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/System/Info/Public":
					_, _ = w.Write([]byte(`{"Id":"server"}`))
				case "/Users/AuthenticateByName":
					_, _ = w.Write([]byte(auth))
				case "/Sessions/Logout":
					logout++
					if got := r.Header.Get("X-Emby-Authorization"); !strings.Contains(got, "Token=") {
						t.Error("logout omitted token")
					}
				default:
					t.Errorf("unexpected %s", r.URL.Path)
				}
			}))
			defer server.Close()
			err := runUpstreamCreate(context.Background(), newTestApp(t), upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "u", BackendPassword: "p"})
			if err == nil || logout != 1 {
				t.Fatalf("err=%v logout=%d", err, logout)
			}
		})
	}
}

func TestUpstreamCreateCancellationAfterAuthDoesNotWriteAndCleansToken(t *testing.T) {
	app := newTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logout := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			_, _ = w.Write([]byte(`{"Id":"server"}`))
		case "/Users/AuthenticateByName":
			_, _ = w.Write([]byte(`{"AccessToken":"new-token","ServerId":"server","User":{"Id":"user"}}`))
		case "/Sessions/Logout":
			logout++
		}
	}))
	defer server.Close()
	controlplane.AfterUpstreamProbe = cancel
	t.Cleanup(func() { controlplane.AfterUpstreamProbe = nil })
	err := runUpstreamCreate(ctx, app, upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "u", BackendPassword: "p"})
	if err == nil || logout != 1 {
		t.Fatalf("err=%v logout=%d", err, logout)
	}
	if count, _ := app.CountRecords(upstreamSources); count != 0 {
		t.Fatalf("cancelled setup wrote %d source records", count)
	}
}

func TestLogoutAcceptsEmptySuccessBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	if err := upstreamRequest(context.Background(), controlplane.NewUpstreamHTTPClient(), http.MethodPost, server.URL, nil, upstreamOptions{}.identity(), "device", "user", "token", &struct{}{}, true); err != nil {
		t.Fatal(err)
	}
}

func TestUpstreamCreateAuthRequestExactBodyAndPublicFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			_, _ = w.Write([]byte(`{"Id":"","ServerId":"server"}`))
		case "/Users/AuthenticateByName":
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"Pw":"p","Username":"u"}` {
				t.Errorf("auth body = %s", body)
			}
			if strings.Contains(r.Header.Get("X-Emby-Authorization"), "Token=") {
				t.Error("auth was not tokenless")
			}
			_, _ = w.Write([]byte(`{"AccessToken":"token","ServerId":" server ","User":{"Id":"user"}}`))
		}
	}))
	defer server.Close()
	if err := runUpstreamCreate(context.Background(), newTestApp(t), upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "u", BackendPassword: "p"}); err != nil {
		t.Fatal(err)
	}
}

func TestUpstreamFingerprintIsTypedAndIncludesEndpointRelation(t *testing.T) {
	app := newTestApp(t)
	sources, _ := app.FindCollectionByNameOrId(upstreamSources)
	endpoints, _ := app.FindCollectionByNameOrId(upstreamEndpoints)
	source := core.NewRecord(sources)
	source.Id = "source"
	source.Set("backend_username", "left\x00right\x01value")
	endpoint := core.NewRecord(endpoints)
	endpoint.Id = "endpoint"
	endpoint.Set("source", "source")
	endpoint.Set("key", "primary")
	endpoint.Set("base_url", "https://emby.example.com")
	first := upstreamFingerprint(upstreamState{Source: source, AllEndpoints: []*core.Record{endpoint}})
	source.Set("backend_username", "left")
	source.Set("backend_password", "right\x01value")
	second := upstreamFingerprint(upstreamState{Source: source, AllEndpoints: []*core.Record{endpoint}})
	if first == second {
		t.Fatal("control-character field drift was not detected")
	}
	source.Set("backend_username", "left\x00right\x01value")
	endpoint.Set("source", "other-source")
	if first == upstreamFingerprint(upstreamState{Source: source, AllEndpoints: []*core.Record{endpoint}}) {
		t.Fatal("endpoint source relation drift was not detected")
	}
}

func TestUpstreamCreateUpdateTriggersRotateAuthTuple(t *testing.T) {
	for _, change := range []string{"url", "username", "password", "user-agent", "client", "device", "version", "missing-token", "missing-user"} {
		t.Run(change, func(t *testing.T) {
			app := newTestApp(t)
			probes := 0
			auths := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/System/Info/Public", "/alternate/System/Info/Public":
					probes++
					_, _ = w.Write([]byte(`{"Id":"server"}`))
				case "/Users/AuthenticateByName", "/alternate/Users/AuthenticateByName":
					probes++
					auths++
					token := "initial-token"
					if auths > 1 {
						token = "new-token"
					}
					_, _ = w.Write([]byte(`{"AccessToken":"` + token + `","ServerId":"server","User":{"Id":"user"}}`))
				case "/Sessions/Logout", "/alternate/Sessions/Logout":
				}
			}))
			defer server.Close()
			opts := upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "u", BackendPassword: "p"}
			if err := runUpstreamCreate(context.Background(), app, opts); err != nil {
				t.Fatal(err)
			}
			source, _ := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
			deviceID := source.GetString("backend_authorization_device_id")
			switch change {
			case "url":
				opts.EmbyBaseURL += "/alternate"
			case "username":
				opts.BackendUsername = "other"
			case "password":
				opts.BackendPassword = "other"
			case "user-agent":
				opts.BackendUserAgent = "other/1"
			case "client":
				opts.BackendAuthorizationClient = "other"
			case "device":
				opts.BackendAuthorizationDevice = "other"
			case "version":
				opts.BackendAuthorizationVersion = "2"
			case "missing-token":
				source.Set("backend_token", "")
				_ = app.Save(source)
			case "missing-user":
				source.Set("backend_user_id", "")
				_ = app.Save(source)
			}
			probes = 0
			if err := runUpstreamCreate(context.Background(), app, opts); err != nil {
				t.Fatal(err)
			}
			if probes != 2 {
				t.Fatalf("trigger %s probes=%d", change, probes)
			}
			source, _ = app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
			if source.GetString("backend_authorization_device_id") == deviceID || source.GetString("auth_generation_id") == "" || source.GetString("backend_token") != "new-token" || source.GetString("backend_user_id") != "user" {
				t.Fatalf("trigger %s did not preserve/refresh source", change)
			}
		})
	}
}

func TestUpstreamCreateCancellationDuringTransactionRollsBack(t *testing.T) {
	app := newTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logout := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			_, _ = w.Write([]byte(`{"Id":"server"}`))
		case "/Users/AuthenticateByName":
			_, _ = w.Write([]byte(`{"AccessToken":"token","ServerId":"server","User":{"Id":"user"}}`))
		case "/Sessions/Logout":
			logout++
		}
	}))
	defer server.Close()
	controlplane.AfterUpstreamSourceSave = cancel
	t.Cleanup(func() { controlplane.AfterUpstreamSourceSave = nil })
	if err := runUpstreamCreate(ctx, app, upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "u", BackendPassword: "p"}); err == nil {
		t.Fatal("cancelled transaction succeeded")
	}
	if count, _ := app.CountRecords(upstreamSources); count != 0 {
		t.Fatalf("source save was not rolled back: %d", count)
	}
	if count, _ := app.CountRecords(upstreamEndpoints); count != 0 {
		t.Fatalf("endpoint save was not rolled back: %d", count)
	}
	if logout != 1 {
		t.Fatalf("expected one detached cleanup request, got %d", logout)
	}
}

func TestUpstreamCreateRejectsSourceAndEndpointFingerprintDriftAndCleansNewToken(t *testing.T) {
	for _, mutate := range []string{"source", "generation", "endpoint"} {
		t.Run(mutate, func(t *testing.T) {
			app, _, opts, closeServer := establishedUpstream(t)
			defer closeServer()
			opts.BackendUsername = "changed"
			source, _ := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
			endpoint, _ := app.FindFirstRecordByData(upstreamEndpoints, "source", source.Id)
			logout := 0
			upstreamTestToken = "new-token"
			controlplane.AfterUpstreamProbe = func() {
				if mutate == "source" {
					source.Set("backend_password", "concurrent")
					_ = app.Save(source)
				} else if mutate == "generation" {
					source.Set("auth_generation_id", "concurrent")
					_ = app.Save(source)
				} else {
					endpoint.Set("key", "concurrent")
					_ = app.Save(endpoint)
				}
			}
			t.Cleanup(func() { controlplane.AfterUpstreamProbe = nil; upstreamTestLogout = nil; upstreamTestToken = "" })
			upstreamTestLogout = func(token string) {
				if token == "new-token" {
					logout++
				}
			}
			if err := runUpstreamCreate(context.Background(), app, opts); err == nil {
				t.Fatal("fingerprint drift was accepted")
			}
			updatedSource, _ := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
			updatedEndpoint, _ := app.FindFirstRecordByData(upstreamEndpoints, "source", source.Id)
			if updatedSource.GetString("backend_token") != "old-token" || (mutate == "source" && updatedSource.GetString("backend_password") != "concurrent") || (mutate == "generation" && updatedSource.GetString("auth_generation_id") != "concurrent") || (mutate == "endpoint" && updatedEndpoint.GetString("key") != "concurrent") {
				t.Fatal("concurrent singleton state was overwritten")
			}
			if logout != 1 {
				t.Fatalf("expected one new-token cleanup, got %d", logout)
			}
		})
	}
}

func TestUpstreamCreateEndpointSaveFailureRollsBackAndCleansNewToken(t *testing.T) {
	app, _, opts, closeServer := establishedUpstream(t)
	defer closeServer()
	opts.BackendUsername = "changed"
	source, _ := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
	endpoint, _ := app.FindFirstRecordByData(upstreamEndpoints, "source", source.Id)
	beforeSource, beforeEndpoint := source.GetString("backend_token"), endpoint.GetString("base_url")
	upstreamTestToken = "new-token"
	logout := 0
	upstreamTestLogout = func(token string) {
		if token == "new-token" {
			logout++
		}
	}
	t.Cleanup(func() { upstreamTestLogout = nil; upstreamTestToken = "" })
	collection, _ := app.FindCollectionByNameOrId(upstreamEndpoints)
	app.OnRecordUpdateExecute(collection.Id).Bind(&hook.Handler[*core.RecordEvent]{Id: "fail_endpoint_save", Priority: 999999, Func: func(*core.RecordEvent) error { return errors.New("endpoint save failed") }})
	t.Cleanup(func() { app.OnRecordUpdateExecute(collection.Id).Unbind("fail_endpoint_save") })
	if err := runUpstreamCreate(context.Background(), app, opts); err == nil {
		t.Fatal("endpoint failure succeeded")
	}
	source, _ = app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
	endpoint, _ = app.FindFirstRecordByData(upstreamEndpoints, "source", source.Id)
	if source.GetString("backend_token") != beforeSource || endpoint.GetString("base_url") != beforeEndpoint || logout != 1 {
		t.Fatal("failed transaction did not fully roll back and clean new token")
	}
}

func TestUpstreamCreateCommitsBeforeOldTokenLogout(t *testing.T) {
	app, _, opts, closeServer := establishedUpstream(t)
	defer closeServer()
	opts.BackendUsername = "changed"
	upstreamTestToken = "new-token"
	var oldLogout, newLogout, sawCommit int
	upstreamTestLogout = func(token string) {
		if token == "old-token" {
			oldLogout++
			source, _ := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
			if source.GetString("backend_token") == "new-token" {
				sawCommit++
			}
		}
		if token == "new-token" {
			newLogout++
		}
	}
	t.Cleanup(func() { upstreamTestLogout = nil; upstreamTestToken = "" })
	if err := runUpstreamCreate(context.Background(), app, opts); err != nil {
		t.Fatal(err)
	}
	if oldLogout != 1 || newLogout != 0 || sawCommit != 1 {
		t.Fatalf("unexpected logout ordering counts: old=%d new=%d committed=%d", oldLogout, newLogout, sawCommit)
	}
}

func TestUpstreamCreateOldTokenCleanupFailuresAreNonFatal(t *testing.T) {
	for _, mode := range []string{"redirect", "status", "transport"} {
		t.Run(mode, func(t *testing.T) {
			app, _, opts, closeOld := establishedUpstream(t)
			defer closeOld()
			newServer := newUpstreamResponder(t)
			defer newServer.Close()
			upstreamTestLogoutMode = mode
			if mode == "transport" {
				closeOld()
			}
			opts.EmbyBaseURL, opts.BackendUsername = newServer.URL, "changed"
			upstreamTestToken = "new-token"
			t.Cleanup(func() { upstreamTestToken = "" })
			if err := runUpstreamCreate(context.Background(), app, opts); err != nil {
				t.Fatal(err)
			}
			source, _ := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
			if source.GetString("backend_token") != "new-token" {
				t.Fatal("cleanup failure rolled back committed token")
			}
		})
	}
}
