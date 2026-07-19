package gateway

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestPlannedPersonalProjectionUsesExplicitSnapshot(t *testing.T) {
	server := NewServer(Config{GatewayServerID: "gateway-server", PublicBaseURL: "https://gateway.test/emby"}, NewMemoryStore())
	session := &Session{GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user"}
	request := httptest.NewRequest(http.MethodGet, "https://gateway.test/emby/Items", nil)
	item := map[string]any{
		"Id": "item", "UserId": "backend-user-new", "ServerId": "backend-server-new",
		"DirectStreamUrl": "/Videos/item/stream?api_key=backend-token-new",
		"Nested":          map[string]any{"Source": "backend"},
	}
	original := clonePlannedPersonalJSONMap(item)
	rewritten := server.rewritePlannedPersonalItem(item, session, upstreamRequestSnapshot{
		baseURL: "https://backend.test/emby", userID: "backend-user-new", serverID: "backend-server-new", token: "backend-token-new",
	}, "gateway-token", request)
	if rewritten["UserId"] != "synthetic-user" || rewritten["ServerId"] != "gateway-server" {
		t.Fatalf("identity rewrite = %#v", rewritten)
	}
	if rewritten["DirectStreamUrl"] == item["DirectStreamUrl"] {
		t.Fatalf("media URL was not rewritten: %#v", rewritten)
	}
	if item["UserId"] != "backend-user-new" {
		t.Fatal("identity rewrite mutated source item")
	}
	if !reflect.DeepEqual(item, original) {
		t.Fatalf("rewrite mutated source item: got %#v want %#v", item, original)
	}
}

func TestPlannedPersonalProjectionOverlaysJoinedUserData(t *testing.T) {
	percentage := 25.0
	liked := false
	joined := []resolvedPersonalItem{{
		item:  map[string]any{"Id": "item-a", "RunTimeTicks": float64(400), "UserData": map[string]any{"Played": true, "IsFavorite": false}},
		state: PlaybackState{ItemID: "item-a", PlaybackPositionTicks: 100, PlayedPercentage: &percentage, PlayCount: 2, IsFavorite: true, Likes: &liked},
	}}
	items, err := (&Server{}).projectPlannedPersonalItems(joined, &Session{GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user"})
	if err != nil {
		t.Fatal(err)
	}
	userData, ok := items[0]["UserData"].(map[string]any)
	if !ok {
		t.Fatalf("UserData = %#v", items[0]["UserData"])
	}
	wantUserData := map[string]any{
		"Played": false, "PlaybackPositionTicks": int64(100), "PlayedPercentage": 25.0,
		"PlayCount": 2, "IsFavorite": true, "Likes": false, "ItemId": "item-a", "Key": "item-a",
	}
	if !reflect.DeepEqual(userData, wantUserData) {
		t.Fatalf("UserData = %#v, want %#v", userData, wantUserData)
	}
	if joined[0].state.GatewayUserID != "" || joined[0].state.SyntheticUserID != "" {
		t.Fatalf("joined state identity was mutated: %#v", joined[0].state)
	}
	if joined[0].item["UserData"].(map[string]any)["Played"] != true {
		t.Fatal("source UserData was mutated")
	}
}

func TestPlannedPersonalProjectionRejectsInvalidJoinedItems(t *testing.T) {
	session := &Session{GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user"}
	cases := []struct {
		name string
		join resolvedPersonalItem
	}{
		{"nil item", resolvedPersonalItem{state: PlaybackState{ItemID: "item"}}},
		{"missing id", resolvedPersonalItem{item: map[string]any{}, state: PlaybackState{ItemID: "item"}}},
		{"empty id", resolvedPersonalItem{item: map[string]any{"Id": ""}, state: PlaybackState{ItemID: "item"}}},
		{"non-string id", resolvedPersonalItem{item: map[string]any{"Id": 42}, state: PlaybackState{ItemID: "item"}}},
		{"malformed id", resolvedPersonalItem{item: map[string]any{"Id": "item/id"}, state: PlaybackState{ItemID: "item/id"}}},
		{"empty state id", resolvedPersonalItem{item: map[string]any{"Id": "item"}}},
		{"mismatched state id", resolvedPersonalItem{item: map[string]any{"Id": "item"}, state: PlaybackState{ItemID: "other"}}},
		{"gateway user mismatch", resolvedPersonalItem{item: map[string]any{"Id": "item"}, state: PlaybackState{ItemID: "item", GatewayUserID: "other"}}},
		{"synthetic user mismatch", resolvedPersonalItem{item: map[string]any{"Id": "item"}, state: PlaybackState{ItemID: "item", SyntheticUserID: "other"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (&Server{}).projectPlannedPersonalItems([]resolvedPersonalItem{tc.join}, session)
			if err == nil || got != nil {
				t.Fatalf("projection = %#v, err = %v", got, err)
			}
		})
	}
	duplicate := []resolvedPersonalItem{
		{item: map[string]any{"Id": "item"}, state: PlaybackState{ItemID: "item"}},
		{item: map[string]any{"Id": "item"}, state: PlaybackState{ItemID: "item"}},
	}
	if got, err := (&Server{}).projectPlannedPersonalItems(duplicate, session); err == nil || got != nil {
		t.Fatalf("duplicate projection = %#v, err = %v", got, err)
	}
	if got, err := (&Server{}).projectPlannedPersonalItems(nil, nil); err == nil || got != nil {
		t.Fatalf("nil session projection = %#v, err = %v", got, err)
	}
}

func TestPlannedPersonalProjectionDoesNotCallStoreOrAliasMaps(t *testing.T) {
	state := PlaybackState{ItemID: "item", GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user"}
	item := map[string]any{"Id": "item", "UserData": map[string]any{"Played": true}, "Nested": map[string]any{"Value": "original"}}
	server := &Server{store: &panickingProjectionStore{}}
	items, err := server.projectPlannedPersonalItems([]resolvedPersonalItem{{item: item, state: state}}, &Session{GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user"})
	if err != nil {
		t.Fatal(err)
	}
	items[0]["UserData"].(map[string]any)["Played"] = false
	items[0]["Nested"].(map[string]any)["Value"] = "changed"
	if item["UserData"].(map[string]any)["Played"] != true || item["Nested"].(map[string]any)["Value"] != "original" {
		t.Fatalf("projection aliased caller map: %#v", item)
	}
}

type panickingProjectionStore struct{ Store }
