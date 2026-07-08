package pbstore

import (
	"context"
	"errors"
	"testing"

	"emby-auth-gateway/internal/gateway"
	_ "emby-auth-gateway/internal/pbmigrations"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func TestRevokeSessionMissingReturnsErrNotFound(t *testing.T) {
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "test",
	})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	defer app.Cleanup()

	if _, err := app.FindCollectionByNameOrId("gateway_sessions"); err != nil {
		t.Fatalf("find gateway_sessions collection: %v", err)
	}

	err = New(app, nil).RevokeSession(context.Background(), "missing-token-hash")
	if !errors.Is(err, gateway.ErrNotFound) {
		t.Fatalf("RevokeSession error = %v, want ErrNotFound", err)
	}
}
