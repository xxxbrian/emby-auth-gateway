package adminquery

import (
	"context"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
)

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
	if err := pbschema.Ensure(app); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	return app
}

func TestListUsersEmpty(t *testing.T) {
	app := newTestApp(t)
	q := New(app, 2)
	users, err := q.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 0 {
		t.Fatalf("len=%d", len(users))
	}
}

func TestGetUpstreamUnconfigured(t *testing.T) {
	app := newTestApp(t)
	q := New(app, 2)
	up, err := q.GetUpstream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if up.Configured {
		t.Fatal("expected unconfigured")
	}
	if up.PasswordSet || up.TokenSet {
		t.Fatal("secrets flags must be false")
	}
}

func TestListAuditWindowValidation(t *testing.T) {
	app := newTestApp(t)
	q := New(app, 2)
	now := time.Now().UTC()
	_, err := q.ListAudit(context.Background(), now.Add(-48*time.Hour), now, 10, "")
	if err == nil {
		t.Fatal("expected window error")
	}
	rows, err := q.ListAudit(context.Background(), now.Add(-time.Hour), now, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if rows == nil {
		t.Fatal("want non-nil slice")
	}
}

func TestListPolicies(t *testing.T) {
	app := newTestApp(t)
	q := New(app, 2)
	policies, err := q.ListPolicies(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) == 0 {
		t.Fatal("expected default policies from schema ensure")
	}
}
