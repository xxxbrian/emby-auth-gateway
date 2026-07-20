package gateway

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestTypedUserProjectionBuildersMatchCurrentMaps(t *testing.T) {
	user := GatewayUser{Username: "alice", SyntheticUserID: "user-1"}
	assertTypedProjectionMatchesMap(t, publicUserWireDTO(user, "server-1"), userDTO(user, "server-1"))
	assertTypedProjectionMatchesMap(t, currentUserWireDTO("alice", "user-1", "server-1"), privateUserDTO("alice", "user-1", "server-1"))

	session := &Session{
		PublicID: "session-1", SyntheticUserID: "user-1", GatewayUsername: "alice",
		LastActivityAt: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
	}
	assertTypedProjectionMatchesMap(t, authenticationResultWireDTO(user, session, "token-1", "server-1"), authenticationResultDTO(user, session, "token-1", "server-1"))
}

func assertTypedProjectionMatchesMap(t *testing.T, typed any, current map[string]any) {
	t.Helper()
	data, err := json.Marshal(typed)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	currentData, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(currentData, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("typed projection mismatch\ngot:  %#v\nwant: %#v", got, want)
	}
}
