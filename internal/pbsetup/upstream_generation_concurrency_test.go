package pbsetup

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
)

type genConcurrentGate struct {
	arrived chan struct{}
	permit  chan struct{}
}

func genConcurrentNewGate() *genConcurrentGate {
	return &genConcurrentGate{arrived: make(chan struct{}, 2), permit: make(chan struct{}, 2)}
}

func (g *genConcurrentGate) wait() {
	g.arrived <- struct{}{}
	<-g.permit
}

func genConcurrentWait(t *testing.T, g *genConcurrentGate) {
	t.Helper()
	for range 2 {
		<-g.arrived
	}
}

func genConcurrentRelease(g *genConcurrentGate, count int) {
	for range count {
		g.permit <- struct{}{}
	}
}

type genConcurrentLogout struct{ token, deviceID string }

type genConcurrentUpstream struct {
	server    *httptest.Server
	sameToken bool
	mu        sync.Mutex
	tokens    []string
	devices   []string
	logouts   []genConcurrentLogout
}

func genConcurrentNewUpstream(t *testing.T, sameToken bool) *genConcurrentUpstream {
	t.Helper()
	u := &genConcurrentUpstream{sameToken: sameToken}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			_, _ = w.Write([]byte(`{"Id":"live-server","ServerName":"live","Version":"4.9"}`))
		case "/Users/AuthenticateByName":
			deviceID := genConcurrentDeviceID(r)
			u.mu.Lock()
			token := "collision-token"
			if !u.sameToken {
				token = fmt.Sprintf("fresh-token-%d", len(u.tokens)+1)
			}
			u.tokens = append(u.tokens, token)
			u.devices = append(u.devices, deviceID)
			u.mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"AccessToken":%q,"ServerId":"live-server","User":{"Id":"live-user"}}`, token)
		case "/Sessions/Logout":
			u.mu.Lock()
			u.logouts = append(u.logouts, genConcurrentLogout{token: genConcurrentToken(r), deviceID: genConcurrentDeviceID(r)})
			u.mu.Unlock()
		}
	}))
	t.Cleanup(u.server.Close)
	return u
}

func genConcurrentDeviceID(r *http.Request) string {
	header := r.Header.Get("X-Emby-Authorization")
	const prefix = `DeviceId="`
	value := strings.SplitN(header, prefix, 2)
	if len(value) != 2 {
		return ""
	}
	return strings.SplitN(value[1], `"`, 2)[0]
}

func genConcurrentToken(r *http.Request) string {
	header := r.Header.Get("X-Emby-Authorization")
	const prefix = `Token="`
	value := strings.SplitN(header, prefix, 2)
	if len(value) != 2 {
		return ""
	}
	return strings.SplitN(value[1], `"`, 2)[0]
}

func (u *genConcurrentUpstream) snapshot() ([]string, []string, []genConcurrentLogout) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.tokens...), append([]string(nil), u.devices...), append([]genConcurrentLogout(nil), u.logouts...)
}

func genConcurrentRunPair(fn func() error) <-chan error {
	results := make(chan error, 2)
	go func() { results <- fn() }()
	go func() { results <- fn() }()
	return results
}

func genConcurrentSetupState(t *testing.T, app core.App, url string) upstreamOptions {
	t.Helper()
	identity := gateway.DefaultBackendClientIdentity()
	sources, err := app.FindCollectionByNameOrId(upstreamSources)
	if err != nil {
		t.Fatal(err)
	}
	source := core.NewRecord(sources)
	for key, value := range map[string]any{
		"key": defaultUpstreamKey, "server_id": "live-server", "server_name": "old", "server_version": "4.8",
		"backend_username": "backend", "backend_password": "old-password", "backend_user_id": "old-user", "backend_token": "old-token",
		"backend_user_agent": identity.UserAgent, "backend_authorization_client": identity.Client, "backend_authorization_device": identity.Device,
		"backend_authorization_device_id": "old-device", "backend_authorization_version": identity.Version,
	} {
		source.Set(key, value)
	}
	if err := app.Save(source); err != nil {
		t.Fatal(err)
	}
	endpoints, err := app.FindCollectionByNameOrId(upstreamEndpoints)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := core.NewRecord(endpoints)
	endpoint.Set("source", source.Id)
	endpoint.Set("key", primaryEndpointKey)
	endpoint.Set("base_url", url)
	endpoint.Set("active", true)
	if err := app.Save(endpoint); err != nil {
		t.Fatal(err)
	}
	return upstreamOptions{EmbyBaseURL: url, BackendUsername: "backend", BackendPassword: "new-password"}
}

func genConcurrentAssertWinner(t *testing.T, app core.App, upstream *genConcurrentUpstream, wantToken string) {
	t.Helper()
	source, err := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
	if err != nil {
		t.Fatal(err)
	}
	tokens, devices, _ := upstream.snapshot()
	if len(tokens) != 2 || len(devices) != 2 || devices[0] == "" || devices[0] == devices[1] {
		t.Fatalf("fresh authentication identities: tokens=%q devices=%q", tokens, devices)
	}
	if source.GetString("backend_token") != wantToken || source.GetString("backend_authorization_device_id") == "" || source.GetString("auth_generation_id") == "" || source.GetString("backend_user_id") != "live-user" {
		t.Fatalf("winner state was not durable: token=%q device=%q generation=%q user=%q", source.GetString("backend_token"), source.GetString("backend_authorization_device_id"), source.GetString("auth_generation_id"), source.GetString("backend_user_id"))
	}
	if source.GetString("backend_authorization_device_id") != devices[0] && source.GetString("backend_authorization_device_id") != devices[1] {
		t.Fatalf("stored device was not from either winner candidate: %q", source.GetString("backend_authorization_device_id"))
	}
}

func genConcurrentAssertOnlyLoserCleanup(t *testing.T, upstream *genConcurrentUpstream, winnerToken string) {
	t.Helper()
	tokens, devices, logouts := upstream.snapshot()
	if len(logouts) != 1 {
		t.Fatalf("logout count=%d, want only losing invocation cleanup: %#v", len(logouts), logouts)
	}
	loser := 0
	if tokens[0] == winnerToken {
		loser = 1
	}
	if logouts[0].token != tokens[loser] || logouts[0].deviceID != devices[loser] || logouts[0].token == winnerToken || logouts[0].token == "old-token" {
		t.Fatalf("loser cleanup was not isolated: tokens=%q devices=%q logouts=%#v", tokens, devices, logouts)
	}
}

func TestUpstreamGenerationConcurrencySetupUpdate(t *testing.T) {
	app := newTestApp(t)
	upstream := genConcurrentNewUpstream(t, false)
	opts := genConcurrentSetupState(t, app, upstream.server.URL)
	gate := genConcurrentNewGate()
	afterUpstreamProbe = gate.wait
	t.Cleanup(func() { afterUpstreamProbe = nil })

	results := genConcurrentRunPair(func() error { return runUpstreamCreate(context.Background(), app, opts) })
	genConcurrentWait(t, gate)
	genConcurrentRelease(gate, 2)
	err1, err2 := <-results, <-results
	if (err1 == nil) == (err2 == nil) {
		t.Fatalf("setup results = %v, %v; want one winner and one loser", err1, err2)
	}
	source, _ := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
	genConcurrentAssertWinner(t, app, upstream, source.GetString("backend_token"))
	genConcurrentAssertOnlyLoserCleanup(t, upstream, source.GetString("backend_token"))
}

func TestUpstreamGenerationConcurrencyImportRepair(t *testing.T) {
	upstream := genConcurrentNewUpstream(t, false)
	app, legacyServer, account := laneBLegacy(t, upstream.server.URL)
	source, _ := laneBSingleton(t, app, legacyServer, account, upstream.server.URL)
	source.Set("backend_token", "")
	if err := app.Save(source); err != nil {
		t.Fatal(err)
	}
	gate := genConcurrentNewGate()
	previousMarshal := marshalImportSummary
	marshalImportSummary = func(value any) ([]byte, error) {
		gate.wait()
		return previousMarshal(value)
	}
	t.Cleanup(func() { marshalImportSummary = previousMarshal })

	opts := importLegacyOptions{ServerRecordID: legacyServer.Id, AccountRecordID: account.Id, Apply: true}
	results := genConcurrentRunPair(func() error {
		_, err := runUpstreamImportLegacy(context.Background(), app, opts, &bytes.Buffer{})
		return err
	})
	genConcurrentWait(t, gate)
	genConcurrentRelease(gate, 2)
	err1, err2 := <-results, <-results
	if (err1 == nil) == (err2 == nil) {
		t.Fatalf("import results = %v, %v; want one winner and one loser", err1, err2)
	}
	source, _ = app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
	genConcurrentAssertWinner(t, app, upstream, source.GetString("backend_token"))
	genConcurrentAssertOnlyLoserCleanup(t, upstream, source.GetString("backend_token"))
}

func TestUpstreamGenerationConcurrencyReturnedTokenCollision(t *testing.T) {
	t.Run("setup", func(t *testing.T) {
		app := newTestApp(t)
		upstream := genConcurrentNewUpstream(t, true)
		opts := genConcurrentSetupState(t, app, upstream.server.URL)
		gate := genConcurrentNewGate()
		afterUpstreamProbe = gate.wait
		t.Cleanup(func() { afterUpstreamProbe = nil })

		results := genConcurrentRunPair(func() error { return runUpstreamCreate(context.Background(), app, opts) })
		genConcurrentWait(t, gate)
		genConcurrentRelease(gate, 1)
		winner := <-results
		if winner != nil {
			t.Fatalf("first setup operation failed: %v", winner)
		}
		genConcurrentRelease(gate, 1)
		if err := <-results; !isTokenOwnershipError(err) {
			t.Fatalf("setup loser error = %v, want typed do-not-cleanup ownership error", err)
		}
		genConcurrentAssertWinner(t, app, upstream, "collision-token")
		_, _, logouts := upstream.snapshot()
		if len(logouts) != 0 {
			t.Fatalf("collision setup logged out durable winner token: %#v", logouts)
		}
	})

	t.Run("import", func(t *testing.T) {
		upstream := genConcurrentNewUpstream(t, true)
		app, legacyServer, account := laneBLegacy(t, upstream.server.URL)
		source, _ := laneBSingleton(t, app, legacyServer, account, upstream.server.URL)
		source.Set("backend_token", "")
		if err := app.Save(source); err != nil {
			t.Fatal(err)
		}
		gate := genConcurrentNewGate()
		previousMarshal := marshalImportSummary
		marshalImportSummary = func(value any) ([]byte, error) {
			gate.wait()
			return previousMarshal(value)
		}
		t.Cleanup(func() { marshalImportSummary = previousMarshal })

		opts := importLegacyOptions{ServerRecordID: legacyServer.Id, AccountRecordID: account.Id, Apply: true}
		results := genConcurrentRunPair(func() error {
			_, err := runUpstreamImportLegacy(context.Background(), app, opts, &bytes.Buffer{})
			return err
		})
		genConcurrentWait(t, gate)
		genConcurrentRelease(gate, 1)
		if err := <-results; err != nil {
			t.Fatalf("first import operation failed: %v", err)
		}
		genConcurrentRelease(gate, 1)
		if err := <-results; !isTokenOwnershipError(err) {
			t.Fatalf("import loser error = %v, want typed do-not-cleanup ownership error", err)
		}
		genConcurrentAssertWinner(t, app, upstream, "collision-token")
		_, _, logouts := upstream.snapshot()
		if len(logouts) != 0 {
			t.Fatalf("collision import logged out durable winner token: %#v", logouts)
		}
	})
}
