package pbsetup

import (
	"context"
	"strings"
	"testing"

	"github.com/xxxbrian/emby-auth-gateway/internal/controlplane"
)

func TestGenerationRotatesOnUpdatesAndStaysOnNoop(t *testing.T) {
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
}

func TestGenerationOnlyDriftCleansOnlyInvocationToken(t *testing.T) {
	app, _, opts, closeServer := establishedUpstream(t)
	defer closeServer()
	opts.BackendUsername, upstreamTestToken = "changed", "new-token"
	source, _ := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
	logouts := []string{}
	upstreamTestLogout = func(token string) { logouts = append(logouts, token) }
	controlplane.AfterUpstreamProbe = func() {
		record, _ := app.FindRecordById(upstreamSources, source.Id)
		record.Set("auth_generation_id", "concurrent-generation")
		_ = app.Save(record)
	}
	t.Cleanup(func() { controlplane.AfterUpstreamProbe = nil; upstreamTestLogout = nil; upstreamTestToken = "" })
	if err := runUpstreamCreate(context.Background(), app, opts); err == nil {
		t.Fatal("generation-only drift was accepted")
	}
	source, _ = app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
	if source.GetString("backend_token") != "old-token" || source.GetString("auth_generation_id") != "concurrent-generation" || strings.Join(logouts, ",") != "new-token" {
		t.Fatalf("source token=%q generation=%q logouts=%q", source.GetString("backend_token"), source.GetString("auth_generation_id"), logouts)
	}
}

func genCollisionAssertRotatedTuple(t *testing.T, source interface{ GetString(string) string }, oldGeneration, token, user string) {
	t.Helper()
	if source.GetString("backend_token") != token || source.GetString("backend_user_id") != user || source.GetString("backend_authorization_device_id") == "" || source.GetString("auth_generation_id") == "" || source.GetString("auth_generation_id") == oldGeneration {
		t.Fatalf("auth tuple was not atomically refreshed")
	}
}
