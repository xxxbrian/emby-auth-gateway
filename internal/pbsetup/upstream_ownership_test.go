package pbsetup

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xxxbrian/emby-auth-gateway/internal/controlplane"

	"github.com/pocketbase/pocketbase/core"
)

func TestClassifyTokenOwnershipProtectsCurrentSourceToken(t *testing.T) {
	app, _, _, closeServer := establishedUpstream(t)
	defer closeServer()
	source, err := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		token string
		want  tokenOwnership
		err   bool
	}{{"old-token", tokenOwnershipProtected, true}, {"", tokenOwnershipUnknown, false}, {"new-token", tokenOwnershipInvocation, false}} {
		got, err := classifyTokenOwnership(app, test.token)
		if got != test.want || (err != nil) != test.err {
			t.Fatalf("token %q: got (%v, %v), want (%v, error=%v)", test.token, got, err, test.want, test.err)
		}
	}
	if source.GetString("backend_token") != "old-token" {
		t.Fatal("unexpected source fixture token")
	}
}

func TestCurrentSourceTokenCollisionDoesNotLogout(t *testing.T) {
	app, _, opts, closeServer := establishedUpstream(t)
	defer closeServer()
	logouts := 0
	upstreamTestLogout = func(string) { logouts++ }
	t.Cleanup(func() { upstreamTestLogout = nil })
	opts.BackendUsername = "changed"
	if err := runUpstreamCreate(context.Background(), app, opts); !isTokenOwnershipError(err) {
		t.Fatalf("error=%v, want ownership collision", err)
	}
	if logouts != 0 {
		t.Fatalf("current source token was logged out %d times", logouts)
	}
}

func TestCurrentSourceOwnershipQueryFailureFailsClosed(t *testing.T) {
	app := newTestApp(t)
	logouts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			_, _ = w.Write([]byte(`{"Id":"server"}`))
		case "/Users/AuthenticateByName":
			_, _ = w.Write([]byte(`{"AccessToken":"new-token","ServerId":"server","User":{"Id":"user"}}`))
		case "/Sessions/Logout":
			logouts++
		}
	}))
	defer server.Close()
	lookupErr := errors.New("table not found")
	controlplane.ReadCurrentTokenSource = func(core.App) (*core.Record, error) { return nil, lookupErr }
	t.Cleanup(func() {
		controlplane.ReadCurrentTokenSource = func(app core.App) (*core.Record, error) {
			return app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
		}
	})
	err := runUpstreamCreate(context.Background(), app, upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "u", BackendPassword: "p"})
	if !isTokenOwnershipError(err) || !errors.Is(err, lookupErr) || logouts != 0 {
		t.Fatalf("error=%v logouts=%d", err, logouts)
	}
	if sources, _ := app.CountRecords(upstreamSources); sources != 0 {
		t.Fatalf("ownership failure wrote %d sources", sources)
	}
	if endpoints, _ := app.CountRecords(upstreamEndpoints); endpoints != 0 {
		t.Fatalf("ownership failure wrote %d endpoints", endpoints)
	}
}

func TestPrimaryStateLoaderFailureStopsBeforeProbe(t *testing.T) {
	app := newTestApp(t)
	requests := 0
	logouts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path == "/Sessions/Logout" {
			logouts++
		}
	}))
	defer server.Close()
	stateErr := errors.New("table not found")
	controlplane.LoadUpstreamStateForCreate = func(core.App) (upstreamState, error) { return upstreamState{}, stateErr }
	t.Cleanup(func() { controlplane.LoadUpstreamStateForCreate = loadUpstreamState })

	err := runUpstreamCreate(context.Background(), app, upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "u", BackendPassword: "p"})
	if !errors.Is(err, stateErr) {
		t.Fatalf("error=%v, want preserved state loader failure", err)
	}
	if requests != 0 || logouts != 0 {
		t.Fatalf("requests=%d logouts=%d, want no upstream side effects", requests, logouts)
	}
	if sources, _ := app.CountRecords(upstreamSources); sources != 0 {
		t.Fatalf("state loader failure wrote %d sources", sources)
	}
	if endpoints, _ := app.CountRecords(upstreamEndpoints); endpoints != 0 {
		t.Fatalf("state loader failure wrote %d endpoints", endpoints)
	}
}

func TestPrimaryStateLoaderSQLNoRowsAllowsFreshCreate(t *testing.T) {
	app := newTestApp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			_, _ = w.Write([]byte(`{"Id":"server"}`))
		case "/Users/AuthenticateByName":
			_, _ = w.Write([]byte(`{"AccessToken":"new-token","ServerId":"server","User":{"Id":"user"}}`))
		}
	}))
	defer server.Close()
	if err := runUpstreamCreate(context.Background(), app, upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "u", BackendPassword: "p"}); err != nil {
		t.Fatal(err)
	}
	if sources, _ := app.CountRecords(upstreamSources); sources != 1 {
		t.Fatalf("source count=%d, want 1", sources)
	}
}

func TestCurrentSourceSQLNoRowsAllowsFreshCreate(t *testing.T) {
	app := newTestApp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			_, _ = w.Write([]byte(`{"Id":"server"}`))
		case "/Users/AuthenticateByName":
			_, _ = w.Write([]byte(`{"AccessToken":"new-token","ServerId":"server","User":{"Id":"user"}}`))
		}
	}))
	defer server.Close()
	controlplane.ReadCurrentTokenSource = func(core.App) (*core.Record, error) { return nil, sql.ErrNoRows }
	t.Cleanup(func() {
		controlplane.ReadCurrentTokenSource = func(app core.App) (*core.Record, error) {
			return app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
		}
	})
	if err := runUpstreamCreate(context.Background(), app, upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "u", BackendPassword: "p"}); err != nil {
		t.Fatal(err)
	}
	if sources, _ := app.CountRecords(upstreamSources); sources != 1 {
		t.Fatalf("source count=%d, want 1", sources)
	}
}
