package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPhase7ResponseProjectionIsExplicit(t *testing.T) {
	if (responseProjection{}).valid() {
		t.Fatal("zero response projection must be invalid")
	}
	kinds := []responseProjectionKind{
		responseProjectionOpaque,
		responseProjectionBaseItem,
		responseProjectionBaseItemEnvelope,
		responseProjectionBaseItemArray,
		responseProjectionSystemInfo,
		responseProjectionPlaybackInfo,
		responseProjectionBaseItemEnvelopeArray,
		responseProjectionAllThemeMedia,
		responseProjectionLiveStreamResponse,
		responseProjectionMediaSource,
	}
	for _, kind := range kinds {
		projection := newResponseProjection(kind)
		if !projection.valid() {
			t.Fatalf("projection kind %d is invalid", kind)
		}
		if got := projection.declaredBaseItemArray(); got != (kind == responseProjectionBaseItemArray) {
			t.Fatalf("projection kind %d array declaration = %v", kind, got)
		}
	}
}

func TestPhase7RawJSONObjectRejectsDuplicateKeys(t *testing.T) {
	tests := []struct {
		name string
		data string
		err  error
	}{
		{name: "exact", data: `{"Id":"a","Id":"b"}`, err: errDuplicateJSONKey},
		{name: "nested", data: `{"Plugin":{"Count":1,"Count":2}}`, err: errDuplicateJSONKey},
		{name: "known case collision", data: `{"Id":"a","id":"b"}`, err: errKnownFieldCollision},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseRawJSONObject([]byte(test.data), "Id", "UserData")
			if !errors.Is(err, test.err) {
				t.Fatalf("error = %v, want %v", err, test.err)
			}
		})
	}
}

func TestPhase7RawJSONObjectPreservesUnknownValuesAndPrecision(t *testing.T) {
	doc, err := parseRawJSONObject([]byte(`{"Id":"item-1","PluginCounter":9007199254740993,"NullValue":null,"FalseValue":false,"ZeroValue":0,"EmptyValue":"","Nested":{"opaque":1}}`), "Id", "UserData")
	if err != nil {
		t.Fatal(err)
	}
	assertRawField := func(name, want string) {
		t.Helper()
		got, ok := doc.Get(name)
		if !ok || string(got) != want {
			t.Fatalf("field %s = %q, %v; want %q", name, got, ok, want)
		}
	}
	assertRawField("PluginCounter", "9007199254740993")
	assertRawField("NullValue", "null")
	assertRawField("FalseValue", "false")
	assertRawField("ZeroValue", "0")
	assertRawField("EmptyValue", `""`)
	if _, ok := doc.Get("MissingValue"); ok {
		t.Fatal("missing field reported present")
	}

	clone := doc.Clone()
	if err := clone.Set("id", json.RawMessage(`"other"`)); !errors.Is(err, errKnownFieldCollision) {
		t.Fatalf("Set known-field collision error = %v", err)
	}
	if err := clone.SetSemantic("UserData", json.RawMessage(`{"Played":false}`)); err != nil {
		t.Fatal(err)
	}
	clone.Remove("Id")
	if _, ok := doc.Get("Id"); !ok {
		t.Fatal("clone mutation changed original")
	}
	encoded, err := clone.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range [][]byte{[]byte(`"PluginCounter":9007199254740993`), []byte(`"Nested":{"opaque":1}`), []byte(`"UserData":{"Played":false}`)} {
		if !bytes.Contains(encoded, fragment) {
			t.Fatalf("marshal dropped %s: %s", fragment, encoded)
		}
	}
}

func TestPhase7CredentialSafetyDetectsRawAndEscapedSecrets(t *testing.T) {
	secret := "backend-token"
	tests := []struct {
		name      string
		data      string
		validator string
		secrets   []string
		want      error
	}{
		{name: "literal text", data: "url?api_key=backend-token", want: errCredentialUnsafe},
		{name: "literal ordinary JSON", data: `{"Token":"backend-token"}`, validator: "json", want: errCredentialUnsafe},
		{name: "JSON escaped", data: `{"Token":"backend\u002dtoken"}`, validator: "json", want: errCredentialUnsafe},
		{name: "mixed-case percent hex", data: "url?api_key=backend%2dtoken", want: errCredentialUnsafe},
		{name: "one-pass percent token", data: "url?api_key=backend%25token", secrets: []string{"backend%token"}, want: errCredentialUnsafe},
		{name: "double encoded is not recursively decoded", data: "url?api_key=backend%252Dtoken"},
		{name: "plus is not space", data: "signature=backend+token", secrets: []string{"backend token"}},
		{name: "safe signature", data: "signature=sha256%3Aabc123"},
		{name: "invalid percent escape", data: "url?signature=safe%ZZvalue"},
		{name: "invalid percent escape still gets literal scan", data: "url?signature=%ZZbackend-token", want: errCredentialUnsafe},
		{name: "empty secret ignored", data: `{"Token":"safe"}`, validator: "json", secrets: []string{""}},
		{name: "percent encoded ordinary JSON body", data: `{"Token":"backend%2Dtoken"}`, validator: "json", want: errCredentialUnsafe},
		{name: "percent encoded opaque JSON body", data: `{"Token":"backend%2Dtoken"}`, validator: "opaque", want: errCredentialUnsafe},
		{name: "bounded", data: "12345", want: errDocumentTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := []byte(test.data)
			data := append([]byte(nil), original...)
			max := int64(len(data))
			secrets := test.secrets
			if secrets == nil {
				secrets = []string{"", secret}
			}
			if test.name == "bounded" {
				max = 4
			}
			var err error
			switch test.validator {
			case "json":
				err = validateCredentialSafeJSON(data, max, secrets...)
			case "opaque":
				err = validateCredentialSafeOpaqueJSON(data, max, secrets...)
			default:
				err = validateCredentialSafeText(data, max, secrets...)
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if !bytes.Equal(data, original) {
				t.Fatalf("validator mutated input: %q", data)
			}
		})
	}
}

func TestPhase7CredentialSafeHeaderValue(t *testing.T) {
	secret := "backend-token"
	tests := []struct {
		name  string
		value string
		want  error
	}{
		{name: "literal", value: "Bearer backend-token", want: errCredentialUnsafe},
		{name: "JSON escaped", value: `Bearer backend\u002dtoken`, want: errCredentialUnsafe},
		{name: "percent encoded", value: "Bearer backend%2Dtoken", want: errCredentialUnsafe},
		{name: "safe", value: "Bearer gateway-token"},
		{name: "empty secret ignored", value: "safe"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			secrets := []string{"", secret}
			if test.name == "empty secret ignored" {
				secrets = []string{""}
			}
			if err := validateCredentialSafeHeaderValue(test.value, secrets...); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestPhase7CredentialSafeOpaqueJSONPreservesExactBytes(t *testing.T) {
	input := []byte(" {\n  \"Unknown\":1, \"Unknown\":2\n}\n")
	original := append([]byte(nil), input...)
	if err := validateCredentialSafeOpaqueJSON(input, int64(len(input)), "backend-token"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(input, original) {
		t.Fatalf("validator mutated input: %q", input)
	}

	if err := validateCredentialSafeOpaqueJSON([]byte(`{"Unknown":"backend\u002dtoken"}`), 1024, "backend-token"); !errors.Is(err, errCredentialUnsafe) {
		t.Fatalf("escaped credential error = %v", err)
	}
	for _, data := range []string{`{"Unknown":`, `{} {}`} {
		if err := validateCredentialSafeOpaqueJSON([]byte(data), 1024, "backend-token"); err == nil {
			t.Fatalf("invalid JSON accepted: %s", data)
		}
	}
}

func TestPhase7ClosedDTOGoldens(t *testing.T) {
	zeroInt64 := int64(0)
	zeroInt := 0
	zeroFloat := float64(0)
	falseValue := false
	zeroPosition := int64(0)
	values := map[string]any{
		"public_user.json":  embyUser{Name: "alice", ServerID: "gateway-server", ServerName: "Emby Gateway", ID: "user-public", HasPassword: true, HasConfiguredPassword: true},
		"private_user.json": phase7PrivateUser(),
		"session_idle.json": phase7Session(false),
		"session_active.json": func() embySessionInfo {
			session := phase7Session(true)
			session.PlayState = embyPlayState{PositionTicks: &zeroPosition, VolumeLevel: &zeroInt, Shuffle: &falseValue}
			session.NowPlayingItem = json.RawMessage(`{"Id":"item-1","PluginCounter":9007199254740993}`)
			return session
		}(),
		"authentication_result.json":        embyAuthenticationResult{AccessToken: "gateway-token", ServerID: "gateway-server", User: phase7PrivateUser(), SessionInfo: phase7Session(false)},
		"user_item_data.json":               embyUserItemData{PlaybackPositionTicks: &zeroInt64, PlayCount: &zeroInt, IsFavorite: &falseValue, Played: &falseValue, PlayedPercentage: &zeroFloat, LastPlayedDate: stringPointer("2026-07-20T12:00:00Z"), UnplayedItemCount: &zeroInt, Likes: json.RawMessage("null"), Key: "item-1", ItemID: "item-1"},
		"query_result.json":                 embyLocalQueryResult{},
		"system_info.json":                  embySystemInfo{ID: "gateway-server", ServerID: "gateway-server", ServerName: "Emby Gateway", Version: "4.9.5.0", LocalAddress: "https://gateway.example/emby", WanAddress: "https://gateway.example/emby", RemoteAddresses: nonNilStrings{"https://gateway.example/emby"}, LocalAddresses: nonNilStrings{"https://gateway.example/emby"}},
		"registration_validate_device.json": registrationValidateDevice{CacheExpirationDays: 233, Message: "Device Valid", ResultCode: "GOOD"},
		"registration_validate.json":        registrationValidate{Registered: true, Expires: "2333-10-01"},
		"registration_status.json":          registrationStatus{PlanType: "Lifetime"},
	}
	for filename, value := range values {
		t.Run(filename, func(t *testing.T) {
			got, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", "phase7", filename))
			if err != nil {
				t.Fatal(err)
			}
			want = bytes.TrimSpace(want)
			if !bytes.Equal(got, want) {
				t.Fatalf("golden mismatch\ngot:  %s\nwant: %s", got, want)
			}
		})
	}
}

func TestPhase7ClosedDTOOmissionAndOpaqueSlots(t *testing.T) {
	data, err := json.Marshal(embyClientCapabilities{DeviceProfile: json.RawMessage(`{"Unknown":9007199254740993}`)})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"PlayableMediaTypes":[],"SupportedCommands":[],"SupportsMediaControl":false,"SupportsSync":false,"DeviceProfile":{"Unknown":9007199254740993}}`
	if string(data) != want {
		t.Fatalf("capabilities = %s, want %s", data, want)
	}
	data, err = json.Marshal(embyUserItemData{})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{}` {
		t.Fatalf("empty user data = %s", data)
	}
}

func phase7Session(active bool) embySessionInfo {
	session := embySessionInfo{
		ID: "session-0123456789abcdef0123456789abcdef", ServerID: "gateway-server", UserID: "user-public", UserName: "alice",
		Client: "Emby Web", DeviceName: "Browser", DeviceID: "device-1", ApplicationVersion: "4.9.5.0",
		LastActivityDate: "2026-07-20T12:00:00Z", PlayState: embyPlayState{},
	}
	if active {
		session.SupportedCommands = nonNilStrings{"DisplayContent"}
		session.PlayableMediaTypes = nonNilStrings{"Video"}
	}
	return session
}

func phase7PrivateUser() embyUser {
	return embyUser{
		Name: "alice", ServerID: "gateway-server", ServerName: "Emby Gateway", ID: "user-public", HasPassword: true, HasConfiguredPassword: true,
		Configuration: &embyUserConfiguration{
			PlayDefaultAudioTrack: true, SubtitleMode: "Smart", RememberAudioSelections: true, RememberSubtitleSelections: true,
			EnableNextEpisodeAutoPlay: true, HidePlayedInLatest: true,
		},
		Policy: &embyUserPolicy{
			EnableUserPreferenceAccess: true, EnableRemoteAccess: true, EnableMediaPlayback: true,
			EnableAudioPlaybackTranscoding: true, EnableVideoPlaybackTranscoding: true, EnablePlaybackRemuxing: true,
			EnableContentDownloading: true, EnableAllChannels: true, EnableAllFolders: true, EnableAllDevices: true,
		},
	}
}

func stringPointer(value string) *string {
	return &value
}
