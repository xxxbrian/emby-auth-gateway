package pbsetup

import (
	"context"
	"testing"
)

func TestSetupUserUpsertsWithoutMappingsOrPasswordRehash(t *testing.T) {
	app := newTestApp(t)
	opts := userOptions{GatewayUsername: "alice", GatewayPassword: "password", SyntheticUserID: "synthetic"}
	if err := runUser(context.Background(), app, opts); err != nil {
		t.Fatal(err)
	}
	user, err := app.FindFirstRecordByData("users", "username", "alice")
	if err != nil {
		t.Fatal(err)
	}
	id, hash := user.Id, user.GetString("password")
	if err := runUser(context.Background(), app, opts); err != nil {
		t.Fatal(err)
	}
	user, _ = app.FindFirstRecordByData("users", "username", "alice")
	if user.Id != id || user.GetString("password") != hash || !user.GetBool("enabled") || !user.Verified() {
		t.Fatalf("user changed unexpectedly: %#v", user)
	}
	mappings, err := app.CountRecords("user_mappings")
	if err != nil || mappings != 0 {
		t.Fatalf("user setup wrote mappings: %d, %v", mappings, err)
	}
}
