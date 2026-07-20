package gateway

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestSessionInfoWireDTOIdleAndActive(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	session := &Session{
		PublicID: "session-1", SyntheticUserID: "user-1", GatewayUsername: "alice",
		Client: "Client", Device: "Device", DeviceID: "device-1", Version: "1.0",
		LastActivityAt: now,
	}

	idle, err := json.Marshal(sessionInfoWireDTO(session, "server-1", nil, nil, false))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(idle, []byte(`"NowPlayingItem"`)) || bytes.Contains(idle, []byte(`"PositionTicks"`)) {
		t.Fatalf("idle session = %s", idle)
	}
	for _, required := range [][]byte{[]byte(`"SupportedCommands":[]`), []byte(`"PlayableMediaTypes":[]`), []byte(`"AdditionalUsers":[]`)} {
		if !bytes.Contains(idle, required) {
			t.Fatalf("idle session missing %s: %s", required, idle)
		}
	}

	zero := int64(0)
	shuffle := false
	current := &CurrentPlayback{
		ItemID: "item-1", ItemSnapshot: PlaybackItemSnapshot{ID: "item-1", Name: "Movie"},
		PlayState: PlaybackPlayState{PositionTicks: &zero, Shuffle: &shuffle},
	}
	active, err := json.Marshal(sessionInfoWireDTO(session, "server-1", current, nil, true))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range [][]byte{[]byte(`"PositionTicks":0`), []byte(`"Shuffle":false`), []byte(`"SupportsRemoteControl":true`), []byte(`"NowPlayingItem":{"Id":"item-1","Name":"Movie"}`)} {
		if !bytes.Contains(active, required) {
			t.Fatalf("active session missing %s: %s", required, active)
		}
	}
}

func TestMergeNowPlayingItemUserDataPreservesRawExtensions(t *testing.T) {
	played := false
	raw := json.RawMessage(`{"PluginCounter":9007199254740993,"Nested":{"Unknown":null},"Id":"item-1"}`)
	got := mergeNowPlayingItemUserData(raw, &embyUserItemData{Played: &played})
	for _, required := range [][]byte{[]byte(`"PluginCounter":9007199254740993`), []byte(`"Nested":{"Unknown":null}`), []byte(`"UserData":{"Played":false}`)} {
		if !bytes.Contains(got, required) {
			t.Fatalf("merged item missing %s: %s", required, got)
		}
	}
}

func TestClientCapabilitiesWireDTORequiredArraysAndOpaqueProfile(t *testing.T) {
	got, err := json.Marshal(embyClientCapabilities{DeviceProfile: json.RawMessage(`{"PluginLimit":9007199254740993}`)})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"PlayableMediaTypes":[],"SupportedCommands":[],"SupportsMediaControl":false,"SupportsSync":false,"DeviceProfile":{"PluginLimit":9007199254740993}}`
	if string(got) != want {
		t.Fatalf("capabilities = %s, want %s", got, want)
	}
}
