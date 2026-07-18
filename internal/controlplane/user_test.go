package controlplane

import (
	"context"
	"errors"
	"testing"

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

func TestCreateUserFailsIfExistsWithoutChangingPassword(t *testing.T) {
	app := newTestApp(t)
	in := UpsertUserInput{
		Username:        "alice",
		Password:        "first-password",
		SyntheticUserID: "synthetic-alice",
	}
	if err := CreateUser(context.Background(), app, in); err != nil {
		t.Fatalf("first create: %v", err)
	}
	rec, err := app.FindFirstRecordByData("users", "username", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !rec.ValidatePassword("first-password") {
		t.Fatal("expected first password to validate")
	}
	hash := rec.GetString("password")

	err = CreateUser(context.Background(), app, UpsertUserInput{
		Username:        "alice",
		Password:        "second-password",
		SyntheticUserID: "synthetic-alice-2",
	})
	if !errors.Is(err, ErrUserExists) {
		t.Fatalf("second create: got %v, want ErrUserExists", err)
	}

	rec, err = app.FindFirstRecordByData("users", "username", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if rec.GetString("password") != hash {
		t.Fatal("password hash changed on duplicate create")
	}
	if !rec.ValidatePassword("first-password") {
		t.Fatal("original password no longer validates")
	}
	if rec.ValidatePassword("second-password") {
		t.Fatal("second password must not have been applied")
	}
	if rec.GetString("synthetic_user_id") != "synthetic-alice" {
		t.Fatalf("synthetic id changed: %q", rec.GetString("synthetic_user_id"))
	}
}

func TestUpsertUserStillUpdatesPassword(t *testing.T) {
	app := newTestApp(t)
	in := UpsertUserInput{
		Username:        "bob",
		Password:        "first-password",
		SyntheticUserID: "synthetic-bob",
	}
	if err := UpsertUser(context.Background(), app, in); err != nil {
		t.Fatal(err)
	}
	if err := UpsertUser(context.Background(), app, UpsertUserInput{
		Username:        "bob",
		Password:        "second-password",
		SyntheticUserID: "synthetic-bob",
	}); err != nil {
		t.Fatal(err)
	}
	rec, err := app.FindFirstRecordByData("users", "username", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if !rec.ValidatePassword("second-password") {
		t.Fatal("upsert should update password")
	}
}
