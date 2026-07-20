package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestControlledBaseItemProjectionPreservesRawFieldsAndPresence(t *testing.T) {
	input := []byte(`{"Id":"item","ServerId":"backend-server","UserId":"backend-user","Big":9223372036854775807123,"Null":null,"False":false,"Zero":0,"Enum":"FutureValue","Nested":{"Value":9007199254740993}}`)
	output, err := projectResponseDocument(input, newResponseProjection(responseProjectionBaseItem), projectionTestContext())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parseRawJSONObject(output, baseItemKnownFields...)
	if err != nil {
		t.Fatal(err)
	}
	assertRawField(t, doc, "Big", `9223372036854775807123`)
	assertRawField(t, doc, "Null", `null`)
	assertRawField(t, doc, "False", `false`)
	assertRawField(t, doc, "Zero", `0`)
	assertRawField(t, doc, "Enum", `"FutureValue"`)
	assertRawField(t, doc, "Nested", `{"Value":9007199254740993}`)
	assertRawField(t, doc, "ServerId", `"gateway-server"`)
	assertRawField(t, doc, "UserId", `"synthetic-user"`)
	if _, ok := doc.GetFold("Omitted"); ok {
		t.Fatal("omitted field was synthesized")
	}
}

func TestControlledProjectionSelectsOnlyDeclaredBaseItemPositions(t *testing.T) {
	ctx := projectionTestContext()
	cases := []struct {
		name       string
		kind       responseProjectionKind
		input      string
		wantCount  int
		wantOpaque bool
	}{
		{"direct", responseProjectionBaseItem, `{"Id":"one","ServerId":"backend-server"}`, 1, false},
		{"item", responseProjectionBaseItemEnvelope, `{"Item":{"Id":"one","ServerId":"backend-server"},"Other":{"ServerId":"backend-server"}}`, 1, false},
		{"items", responseProjectionBaseItemEnvelope, `{"Items":[{"Id":"one","ServerId":"backend-server"},{"Id":"two","ServerId":"backend-server"}]}`, 2, false},
		{"declared array", responseProjectionBaseItemArray, `[{"Id":"one","ServerId":"backend-server"}]`, 1, false},
		{"opaque array", responseProjectionOpaque, `[{"Id":"one","ServerId":"backend-server"}]`, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output, err := projectResponseDocument([]byte(tc.input), newResponseProjection(tc.kind), ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(output), `"gateway-server"`); got != tc.wantCount {
				t.Fatalf("gateway identity count = %d in %s", got, output)
			}
			if tc.wantOpaque && !bytes.Equal(output, []byte(tc.input)) {
				t.Fatalf("opaque array changed: %s", output)
			}
		})
	}
	if _, err := projectResponseDocument([]byte(`[]`), responseProjection{}, ctx); !errors.Is(err, errUnsupportedResponseProjection) {
		t.Fatalf("invalid plan error = %v", err)
	}
}

func TestControlledProjectionRejectsKnownFieldCaseCollisions(t *testing.T) {
	ctx := projectionTestContext()
	inputs := []struct {
		kind responseProjectionKind
		body string
	}{
		{responseProjectionBaseItem, `{"ServerId":"backend-server","serverid":"backend-server"}`},
		{responseProjectionBaseItemEnvelope, `{"Items":[],"items":[]}`},
		{responseProjectionPlaybackInfo, `{"MediaSources":[{"DirectStreamUrl":"/Videos/a/stream","directstreamurl":"/Videos/a/stream"}]}`},
	}
	for _, input := range inputs {
		if _, err := projectResponseDocument([]byte(input.body), newResponseProjection(input.kind), ctx); !errors.Is(err, errKnownFieldCollision) {
			t.Fatalf("projection error = %v for %s", err, input.body)
		}
	}
}

func TestControlledBaseItemProjectionUsesExactIdentityAndKnownURLSlots(t *testing.T) {
	input := []byte(`{
		"Id":"backend-user",
		"ServerId":"prefix-backend-server-suffix",
		"UserId":"backend-user",
		"Name":"backend-user backend-server",
		"DirectStreamUrl":"https://backend.test/emby/Videos/item/stream?api_key=backend-token",
		"UnknownUrl":"https://backend.test/emby/Videos/unknown/stream",
		"ExternalUrls":[{"Url":"https://outside.test/title","Label":"backend-user"}],
		"RemoteTrailers":[{"Url":"/Videos/trailer/stream"}],
		"MediaSources":[{"DirectStreamUrl":"/Videos/item/stream","UnknownUrl":"/Videos/no/stream","MediaStreams":[{"DeliveryUrl":"/Videos/item/subtitles/0/stream","Codec":"backend-user"}]}]
	}`)
	output, err := projectResponseDocument(input, newResponseProjection(responseProjectionBaseItem), projectionTestContext())
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	for _, preserved := range []string{`"Id":"backend-user"`, `"ServerId":"prefix-backend-server-suffix"`, `"Name":"backend-user backend-server"`, `"UnknownUrl":"https://backend.test/emby/Videos/unknown/stream"`, `"Label":"backend-user"`, `"Codec":"backend-user"`} {
		if !strings.Contains(text, preserved) {
			t.Fatalf("missing preserved value %s in %s", preserved, text)
		}
	}
	if !strings.Contains(text, `"UserId":"synthetic-user"`) || strings.Contains(text, "backend-token") {
		t.Fatalf("identity/credential projection failed: %s", text)
	}
	if strings.Count(text, "api_key=gateway-token") < 3 {
		t.Fatalf("known media URLs were not projected: %s", text)
	}
}

func TestControlledBaseItemUserDataOverlayPreservesExtensions(t *testing.T) {
	liked := false
	ctx := projectionTestContext()
	ctx.overlayBaseItem = func(item *baseItemDocument) error {
		return item.overlayPlaybackState(PlaybackState{ItemID: "item", PlaybackPositionTicks: 25, PlayCount: 2, IsFavorite: true, Likes: &liked}, nil)
	}
	output, err := projectResponseDocument([]byte(`{"Id":"item","RunTimeTicks":100,"UserData":{"Played":true,"Likes":true,"PluginValue":9223372036854775808}}`), newResponseProjection(responseProjectionBaseItem), ctx)
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := parseRawJSONObject(output, baseItemKnownFields...)
	raw, _ := doc.GetFold("UserData")
	userData, err := parseRawJSONObject(raw, userDataKnownFields...)
	if err != nil {
		t.Fatal(err)
	}
	assertRawField(t, userData, "Played", `false`)
	assertRawField(t, userData, "PlaybackPositionTicks", `25`)
	assertRawField(t, userData, "PlayedPercentage", `25`)
	assertRawField(t, userData, "Likes", `false`)
	assertRawField(t, userData, "PluginValue", `9223372036854775808`)
}

func TestControlledSystemInfoProjection(t *testing.T) {
	output, err := projectResponseDocument([]byte(`{"Id":"backend-server","ServerId":"backend-server","LocalAddress":null,"WanAddress":"old","RemoteAddresses":[],"Unknown":0}`), newResponseProjection(responseProjectionSystemInfo), projectionTestContext())
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := parseRawJSONObject(output, "Id", "ServerId", "LocalAddress", "WanAddress", "RemoteAddresses", "LocalAddresses")
	assertRawField(t, doc, "Id", `"gateway-server"`)
	assertRawField(t, doc, "ServerId", `"gateway-server"`)
	assertRawField(t, doc, "LocalAddress", `"https://gateway.test/emby"`)
	assertRawField(t, doc, "RemoteAddresses", `["https://gateway.test/emby"]`)
	assertRawField(t, doc, "Unknown", `0`)
	if _, ok := doc.GetFold("LocalAddresses"); ok {
		t.Fatal("omitted address field was synthesized")
	}
}

func TestControlledPlaybackInfoAndSessionNowPlayingProjection(t *testing.T) {
	input := []byte(`{"PlaySessionId":"backend-user","MediaSources":[{"Id":"source","ServerId":"backend-server","DirectStreamUrl":"/Videos/item/stream","MediaStreams":[{"DeliveryUrl":"/Videos/item/subtitles/0/stream","Unknown":null}],"Unknown":false}],"Unknown":0}`)
	output, err := projectResponseDocument(input, newResponseProjection(responseProjectionPlaybackInfo), projectionTestContext())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), `"PlaySessionId":"backend-user"`) || !strings.Contains(string(output), `"Unknown":false`) || !strings.Contains(string(output), `"ServerId":"gateway-server"`) || strings.Count(string(output), "api_key=gateway-token") != 2 {
		t.Fatalf("PlaybackInfo projection = %s", output)
	}
	nowPlaying, err := projectSessionInfoNowPlayingItem(json.RawMessage(`{"Id":"item","ServerId":"backend-server"}`), projectionTestContext())
	if err != nil || !strings.Contains(string(nowPlaying), `"ServerId":"gateway-server"`) {
		t.Fatalf("NowPlayingItem projection = %s, %v", nowPlaying, err)
	}
}

func TestControlledRecommendationAndThemeMediaProjection(t *testing.T) {
	ctx := projectionTestContext()
	tests := []struct {
		name      string
		kind      responseProjectionKind
		input     string
		wantCount int
	}{
		{
			name:      "recommendations",
			kind:      responseProjectionBaseItemEnvelopeArray,
			input:     `[{"BaselineItemName":"A","Items":[{"Id":"one","ServerId":"backend-server"}],"Item":{"ServerId":"backend-server"}},{"Items":[]},{"Other":{"ServerId":"backend-server"}}]`,
			wantCount: 1,
		},
		{
			name: "all theme media",
			kind: responseProjectionAllThemeMedia,
			input: `{"ThemeVideosResult":{"Items":[{"Id":"one","ServerId":"backend-server"}]},` +
				`"ThemeSongsResult":{"Items":[]},"SoundtrackSongsResult":{"Item":{"Id":"two","ServerId":"backend-server"}},` +
				`"Other":{"ServerId":"backend-server"}}`,
			wantCount: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output, err := projectResponseDocument([]byte(test.input), newResponseProjection(test.kind), ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(output), `"ServerId":"gateway-server"`); got != test.wantCount {
				t.Fatalf("projected count = %d, want %d: %s", got, test.wantCount, output)
			}
			if strings.Contains(string(output), `"Items":null`) {
				t.Fatalf("empty Items became null: %s", output)
			}
		})
	}
}

func TestControlledLiveStreamAndMediaSourceProjection(t *testing.T) {
	ctx := projectionTestContext()
	mediaSource := `{"ServerId":"backend-server","DirectStreamUrl":"/Videos/item/stream","TranscodingUrl":null,"LiveStreamUrl":"",` +
		`"MediaStreams":[{"DeliveryUrl":"/Videos/item/subtitles/0/stream","Unknown":false}],"Unknown":9007199254740993}`
	tests := []struct {
		name  string
		kind  responseProjectionKind
		input string
	}{
		{name: "singular root", kind: responseProjectionMediaSource, input: mediaSource},
		{name: "live stream response", kind: responseProjectionLiveStreamResponse, input: `{"MediaSource":` + mediaSource + `,"Unknown":null}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output, err := projectResponseDocument([]byte(test.input), newResponseProjection(test.kind), ctx)
			if err != nil {
				t.Fatal(err)
			}
			text := string(output)
			for _, want := range []string{`"ServerId":"gateway-server"`, `"TranscodingUrl":null`, `"LiveStreamUrl":""`, `"Unknown":9007199254740993`, `"Unknown":false`} {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %s in %s", want, output)
				}
			}
			if strings.Count(text, "api_key=gateway-token") != 2 {
				t.Fatalf("media URLs not projected: %s", output)
			}
		})
	}

	for _, input := range []string{`{"MediaSource":null,"Unknown":1}`, `{"Unknown":1}`} {
		output, err := projectResponseDocument([]byte(input), newResponseProjection(responseProjectionLiveStreamResponse), ctx)
		if err != nil || !strings.Contains(string(output), `"Unknown":1`) {
			t.Fatalf("presence projection = %s, %v", output, err)
		}
	}
}

func TestControlledProjectionPreservesDocumentedEmptyArrays(t *testing.T) {
	ctx := projectionTestContext()
	tests := []struct {
		kind  responseProjectionKind
		input string
	}{
		{responseProjectionBaseItem, `{"MediaSources":[],"MediaStreams":[],"ExternalUrls":[],"RemoteTrailers":[]}`},
		{responseProjectionPlaybackInfo, `{"MediaSources":[]}`},
		{responseProjectionMediaSource, `{"MediaStreams":[]}`},
		{responseProjectionBaseItemArray, `[]`},
		{responseProjectionBaseItemEnvelopeArray, `[]`},
		{responseProjectionAllThemeMedia, `{"ThemeVideosResult":{"Items":[]},"ThemeSongsResult":{"Items":[]},"SoundtrackSongsResult":{"Items":[]}}`},
	}
	for _, test := range tests {
		output, err := projectResponseDocument([]byte(test.input), newResponseProjection(test.kind), ctx)
		if err != nil {
			t.Fatalf("kind %d: %v", test.kind, err)
		}
		if strings.Contains(string(output), "null") {
			t.Fatalf("kind %d changed empty array to null: %s", test.kind, output)
		}
	}
	items, err := parseRawJSONArray([]byte(`[]`))
	if err != nil || items == nil || len(items) != 0 {
		t.Fatalf("empty array = %#v, %v", items, err)
	}
}

func TestProjectedBaseItemDocumentsUsesOnlyDeclaredSlots(t *testing.T) {
	tests := []struct {
		name string
		kind responseProjectionKind
		data string
		want []string
	}{
		{name: "direct", kind: responseProjectionBaseItem, data: `{"Id":"direct"}`, want: []string{"direct"}},
		{name: "envelope", kind: responseProjectionBaseItemEnvelope, data: `{"Item":{"Id":"item"},"Items":[{"Id":"items"}],"Other":{"Id":"guess"}}`, want: []string{"item", "items"}},
		{name: "recommendations", kind: responseProjectionBaseItemEnvelopeArray, data: `[{"Items":[{"Id":"recommendation"}],"Item":{"Id":"guess"},"Other":{"Id":"guess"}}]`, want: []string{"recommendation"}},
		{name: "themes", kind: responseProjectionAllThemeMedia, data: `{"ThemeVideosResult":{"Items":[{"Id":"video"}]},"ThemeSongsResult":{"Items":[{"Id":"song"}]},"Other":{"Items":[{"Id":"guess"}]}}`, want: []string{"video", "song"}},
		{name: "opaque", kind: responseProjectionOpaque, data: `{"Items":[{"Id":"guess"}]}`, want: []string{}},
		{name: "playback info", kind: responseProjectionPlaybackInfo, data: `{"Items":[{"Id":"guess"}]}`, want: []string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			documents, err := projectedBaseItemDocuments([]byte(test.data), newResponseProjection(test.kind))
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(documents))
			for _, raw := range documents {
				doc, err := parseBaseItemDocument(raw)
				if err != nil {
					t.Fatal(err)
				}
				id, _ := doc.itemID()
				got = append(got, id)
			}
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("documents = %v, want %v", got, test.want)
			}
		})
	}
}

func TestOpaqueProjectionPreservesDuplicateUnknownNamesAndExactBytes(t *testing.T) {
	input := []byte(" {\n \"Unknown\":1, \"Unknown\":2\n}\n")
	output, err := projectResponseDocument(input, newResponseProjection(responseProjectionOpaque), projectionTestContext())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output, input) {
		t.Fatalf("opaque projection changed bytes:\n%q\n%q", output, input)
	}
}

func TestControlledProjectionRejectsCredentialsInOpaqueFields(t *testing.T) {
	ctx := projectionTestContext()
	for _, input := range []string{
		`{"Id":"item","Opaque":"backend-token"}`,
		`{"Id":"item","Opaque":"backend-\u0074oken"}`,
	} {
		if _, err := projectResponseDocument([]byte(input), newResponseProjection(responseProjectionBaseItem), ctx); !errors.Is(err, errCredentialUnsafe) {
			t.Fatalf("credential error = %v for %s", err, input)
		}
	}
}

func TestControlledProjectionDoesNotAliasInput(t *testing.T) {
	input := []byte(`{"Id":"item","Unknown":{"Value":1}}`)
	output, err := projectResponseDocument(input, newResponseProjection(responseProjectionBaseItem), projectionTestContext())
	if err != nil {
		t.Fatal(err)
	}
	copyBefore := append([]byte(nil), output...)
	for i := range input {
		input[i] = 'x'
	}
	if !bytes.Equal(output, copyBefore) {
		t.Fatalf("output aliases input: %s", output)
	}
}

func projectionTestContext() responseProjectionContext {
	return responseProjectionContext{
		session:           &Session{SyntheticUserID: "synthetic-user"},
		upstream:          upstreamRequestSnapshot{baseURL: "https://backend.test/emby", serverID: "backend-server", userID: "backend-user", token: "backend-token"},
		gatewayToken:      "gateway-token",
		publicGatewayBase: "https://gateway.test/emby",
		gatewayServerID:   "gateway-server",
	}
}

func assertRawField(t *testing.T, doc *rawJSONObject, name, want string) {
	t.Helper()
	raw, ok := doc.GetFold(name)
	if !ok {
		t.Fatalf("missing field %s", name)
	}
	if string(raw) != want {
		t.Fatalf("%s = %s, want %s", name, raw, want)
	}
}
