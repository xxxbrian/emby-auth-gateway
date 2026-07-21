package routeclass

import (
	"testing"
)

func TestClassifyPublicAndLocal(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		want   Decision
	}{
		{
			name:   "authenticate",
			method: "POST",
			path:   "/Users/AuthenticateByName",
			want:   Decision{Ownership: LocalPublic, Operation: OperationAuthenticate, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "authenticate wrong method",
			method: "GET",
			path:   "/Users/AuthenticateByName",
			want:   Decision{Ownership: LocalPublic, Operation: OperationAuthenticate, MethodAllowed: false, Allow: "POST"},
		},
		{
			name:   "public system info",
			method: "GET",
			path:   "/System/Info/Public",
			want:   Decision{Ownership: LocalPublic, Operation: OperationPublicSystemInfo, MethodAllowed: true, Allow: "GET"},
		},
		{
			name:   "ping get",
			method: "GET",
			path:   "/System/Ping",
			want:   Decision{Ownership: LocalPublic, Operation: OperationPing, MethodAllowed: true, Allow: "GET, POST"},
		},
		{
			name:   "ping post",
			method: "POST",
			path:   "/System/Ping",
			want:   Decision{Ownership: LocalPublic, Operation: OperationPing, MethodAllowed: true, Allow: "GET, POST"},
		},
		{
			name:   "ping wrong method",
			method: "DELETE",
			path:   "/System/Ping",
			want:   Decision{Ownership: LocalPublic, Operation: OperationPing, MethodAllowed: false, Allow: "GET, POST"},
		},
		{
			name:   "public users",
			method: "GET",
			path:   "/Users/Public",
			want:   Decision{Ownership: LocalPublic, Operation: OperationPublicUsers, MethodAllowed: true, Allow: "GET"},
		},
		{
			name:   "current user by id",
			method: "GET",
			path:   "/Users/gateway-user",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationCurrentUser, MethodAllowed: true, Allow: "GET"},
		},
		{
			name:   "current user Me",
			method: "GET",
			path:   "/Users/Me",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationCurrentUser, MethodAllowed: true, Allow: "GET"},
		},
		{
			name:   "deeper user path is not current user",
			method: "GET",
			path:   "/Users/gateway-user/Items",
			want:   Decision{Ownership: MetadataProxy, Operation: OperationMetadataProxy, MethodAllowed: true, Allow: "GET, HEAD"},
		},
		{
			name:   "branding configuration",
			method: "GET",
			path:   "/Branding/Configuration",
			want:   Decision{Ownership: LocalPublic, Operation: OperationBrandingConfiguration, MethodAllowed: true, Allow: "GET"},
		},
		{
			name:   "branding css",
			method: "GET",
			path:   "/Branding/Css.css",
			want:   Decision{Ownership: LocalPublic, Operation: OperationBrandingCSS, MethodAllowed: true, Allow: "GET"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDecision(t, Classify(tc.method, tc.path), tc.want)
		})
	}
}

func TestClassifyExactSessionOperations(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		want   Decision
	}{
		{
			name:   "session list",
			method: "GET",
			path:   "/Sessions",
			want:   Decision{Ownership: LocalSession, Operation: OperationSessionList, MethodAllowed: true, Allow: "GET"},
		},
		{
			name:   "session list wrong method",
			method: "POST",
			path:   "/Sessions",
			want:   Decision{Ownership: LocalSession, Operation: OperationSessionList, MethodAllowed: false, Allow: "GET"},
		},
		{
			name:   "logout",
			method: "POST",
			path:   "/Sessions/Logout",
			want:   Decision{Ownership: LocalSession, Operation: OperationLogout, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "logout wrong method",
			method: "GET",
			path:   "/Sessions/Logout",
			want:   Decision{Ownership: LocalSession, Operation: OperationLogout, MethodAllowed: false, Allow: "POST"},
		},
		{
			name:   "playing",
			method: "POST",
			path:   "/Sessions/Playing",
			want:   Decision{Ownership: LocalSession, Operation: OperationPlaybackReport, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "progress",
			method: "POST",
			path:   "/Sessions/Playing/Progress",
			want:   Decision{Ownership: LocalSession, Operation: OperationPlaybackReport, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "stopped",
			method: "POST",
			path:   "/Sessions/Playing/Stopped",
			want:   Decision{Ownership: LocalSession, Operation: OperationPlaybackReport, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "playback report wrong method",
			method: "GET",
			path:   "/Sessions/Playing/Progress",
			want:   Decision{Ownership: LocalSession, Operation: OperationPlaybackReport, MethodAllowed: false, Allow: "POST"},
		},
		{
			name:   "playback ping",
			method: "POST",
			path:   "/Sessions/Playing/Ping",
			want:   Decision{Ownership: LocalSession, Operation: OperationPlaybackPing, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "playback ping wrong method",
			method: "PUT",
			path:   "/Sessions/Playing/Ping",
			want:   Decision{Ownership: LocalSession, Operation: OperationPlaybackPing, MethodAllowed: false, Allow: "POST"},
		},
		{
			name:   "capabilities",
			method: "POST",
			path:   "/Sessions/Capabilities",
			want:   Decision{Ownership: LocalSession, Operation: OperationCapabilities, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "capabilities full",
			method: "POST",
			path:   "/Sessions/Capabilities/Full",
			want:   Decision{Ownership: LocalSession, Operation: OperationCapabilities, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "capabilities wrong method",
			method: "GET",
			path:   "/Sessions/Capabilities/Full",
			want:   Decision{Ownership: LocalSession, Operation: OperationCapabilities, MethodAllowed: false, Allow: "POST"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDecision(t, Classify(tc.method, tc.path), tc.want)
		})
	}
}

func TestClassifyDeniedSession(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		want   Decision
	}{
		{
			name:   "playqueue get",
			method: "GET",
			path:   "/Sessions/PlayQueue",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: true, Allow: "GET"},
		},
		{
			name:   "playqueue wrong method",
			method: "POST",
			path:   "/Sessions/PlayQueue",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: false, Allow: "GET"},
		},
		{
			name:   "playing pause ambiguous denied",
			method: "POST",
			path:   "/Sessions/Playing/Pause",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: true},
		},
		{
			name:   "unknown descendant",
			method: "GET",
			path:   "/Sessions/SomeUndocumented/Thing",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: true},
		},
		{
			name:   "targeted playing",
			method: "POST",
			path:   "/Sessions/sid-1/Playing",
			want:   Decision{Ownership: LocalSession, Operation: OperationSessionPlay, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "targeted playing command",
			method: "POST",
			path:   "/Sessions/sid-1/Playing/Unpause",
			want:   Decision{Ownership: LocalSession, Operation: OperationSessionPlaystate, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "targeted playing wrong method",
			method: "GET",
			path:   "/Sessions/sid-1/Playing/Unpause",
			want:   Decision{Ownership: LocalSession, Operation: OperationSessionPlaystate, MethodAllowed: false, Allow: "POST"},
		},
		{
			name:   "targeted command",
			method: "POST",
			path:   "/Sessions/sid-1/Command",
			want:   Decision{Ownership: LocalSession, Operation: OperationSessionGeneralCommand, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "targeted command name",
			method: "POST",
			path:   "/Sessions/sid-1/Command/MoveUp",
			want:   Decision{Ownership: LocalSession, Operation: OperationSessionGeneralCommand, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "targeted command wrong method",
			method: "DELETE",
			path:   "/Sessions/sid-1/Command/MoveUp",
			want:   Decision{Ownership: LocalSession, Operation: OperationSessionGeneralCommand, MethodAllowed: false, Allow: "POST"},
		},
		{
			name:   "targeted system",
			method: "POST",
			path:   "/Sessions/sid-1/System/DisplayContent",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "targeted system wrong method",
			method: "GET",
			path:   "/Sessions/sid-1/System/DisplayContent",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: false, Allow: "POST"},
		},
		{
			name:   "targeted message",
			method: "POST",
			path:   "/Sessions/sid-1/Message",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "targeted viewing",
			method: "POST",
			path:   "/Sessions/sid-1/Viewing",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "additional user attach post",
			method: "POST",
			path:   "/Sessions/sid-1/Users/user-2",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: true, Allow: "POST, DELETE"},
		},
		{
			name:   "additional user detach delete",
			method: "DELETE",
			path:   "/Sessions/sid-1/Users/user-2",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: true, Allow: "POST, DELETE"},
		},
		{
			name:   "additional user wrong method",
			method: "GET",
			path:   "/Sessions/sid-1/Users/user-2",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: false, Allow: "POST, DELETE"},
		},
		{
			name:   "additional user delete form",
			method: "POST",
			path:   "/Sessions/sid-1/Users/user-2/Delete",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "additional user delete form wrong method",
			method: "DELETE",
			path:   "/Sessions/sid-1/Users/user-2/Delete",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: false, Allow: "POST"},
		},
		{
			name:   "targeted read remains denied without 405",
			method: "GET",
			path:   "/Sessions/sid-1",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDecision(t, Classify(tc.method, tc.path), tc.want)
		})
	}
}

func TestClassifyNormalizationAndNonSession(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		want   Decision
	}{
		{
			name:   "mixed case session list",
			method: "get",
			path:   "/sessions",
			want:   Decision{Ownership: LocalSession, Operation: OperationSessionList, MethodAllowed: true, Allow: "GET"},
		},
		{
			name:   "trailing slash logout",
			method: "POST",
			path:   "/Sessions/Logout/",
			want:   Decision{Ownership: LocalSession, Operation: OperationLogout, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "question mark is path data not query strip",
			method: "POST",
			path:   "/Sessions/Playing?x",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: true},
		},
		{
			name:   "hash is path data not fragment strip",
			method: "POST",
			path:   "/Sessions/Playing#x",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: true},
		},
		{
			name:   "progress with embedded query chars denied not playback report",
			method: "POST",
			path:   "/Sessions/Playing/Progress?api_key=secret",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: true},
		},
		{
			name:   "trailing space path is not exact session playing",
			method: "POST",
			path:   "/Sessions/Playing ",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: true},
		},
		{
			name:   "leading space path is not exact public system info",
			method: "GET",
			path:   "  /System/Info/Public",
			want:   Decision{Ownership: Unclassified, Operation: OperationUnclassified, MethodAllowed: false},
		},
		{
			name:   "trailing space path is not exact public system info",
			method: "GET",
			path:   "/System/Info/Public  ",
			// trailing space breaks exact Public match; remaining /System/Info* is not an exact template
			want: Decision{Ownership: Unclassified, Operation: OperationUnclassified, MethodAllowed: false},
		},
		{
			name:   "sessionsx is not sessions",
			method: "GET",
			path:   "/SessionsX",
			want:   Decision{Ownership: Unclassified, Operation: OperationUnclassified, MethodAllowed: false},
		},
		{
			name:   "sessionsx descendant is not sessions",
			method: "POST",
			path:   "/SessionsX/Playing",
			want:   Decision{Ownership: Unclassified, Operation: OperationUnclassified, MethodAllowed: false},
		},
		{
			name:   "missing leading slash",
			method: "POST",
			path:   "Users/AuthenticateByName",
			want:   Decision{Ownership: LocalPublic, Operation: OperationAuthenticate, MethodAllowed: true, Allow: "POST"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDecision(t, Classify(tc.method, tc.path), tc.want)
		})
	}
}

func TestClassifyPersonalMetadataMedia(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		want   Decision
	}{
		{
			name:   "display preferences",
			method: "GET",
			path:   "/DisplayPreferences/home",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: true, Allow: "GET, POST"},
		},
		{
			name:   "display preferences usersettings",
			method: "GET",
			path:   "/DisplayPreferences/usersettings",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: true, Allow: "GET, POST"},
		},
		{
			name:   "display preferences patch denied method",
			method: "PATCH",
			path:   "/DisplayPreferences/home",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: false, Allow: "GET, POST"},
		},
		{
			name:   "display preferences deeper unclassified",
			method: "GET",
			path:   "/DisplayPreferences/home/extra",
			want:   Decision{Ownership: Unclassified, Operation: OperationUnclassified, MethodAllowed: false},
		},
		{
			name:   "played items write",
			method: "POST",
			path:   "/Users/u1/PlayedItems/item-1",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: true, Allow: "POST, DELETE"},
		},
		{
			name:   "favorite items write",
			method: "POST",
			path:   "/Users/u1/FavoriteItems/item-1",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: true, Allow: "POST, DELETE"},
		},
		{
			name:   "rating write",
			method: "POST",
			path:   "/Users/u1/Items/item-1/Rating",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: true, Allow: "POST, DELETE"},
		},
		{
			name:   "userdata write",
			method: "POST",
			path:   "/Users/u1/Items/item-1/UserData",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "userdata patch denied method",
			method: "PATCH",
			path:   "/Users/u1/Items/item-1/UserData",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: false, Allow: "POST"},
		},
		{
			name:   "hide from resume unclassified",
			method: "POST",
			path:   "/Users/u1/HideFromResume/item-1",
			want:   Decision{Ownership: Unclassified, Operation: OperationUnclassified, MethodAllowed: false},
		},
		{
			name:   "resume",
			method: "GET",
			path:   "/Users/u1/Items/Resume",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: true, Allow: "GET"},
		},
		{
			name:   "latest",
			method: "GET",
			path:   "/Users/u1/Items/Latest",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: true, Allow: "GET"},
		},
		{
			name:   "next up",
			method: "GET",
			path:   "/Shows/NextUp",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: true, Allow: "GET"},
		},
		{
			name:   "user items list metadata",
			method: "GET",
			path:   "/Users/u1/Items",
			want:   Decision{Ownership: MetadataProxy, Operation: OperationMetadataProxy, MethodAllowed: true, Allow: "GET, HEAD"},
		},
		{
			name:   "playback info",
			method: "GET",
			path:   "/Items/abc/PlaybackInfo",
			want:   Decision{Ownership: MediaProxy, Operation: OperationPlaybackInfo, MethodAllowed: true, Allow: "GET, POST"},
		},
		{
			name:   "video stream admitted",
			method: "GET",
			path:   "/Videos/item1/stream",
			want:   Decision{Ownership: MediaProxy, Operation: OperationMediaProxy, MethodAllowed: true, Allow: "GET, HEAD"},
		},
		{
			name:   "audio stream container admitted",
			method: "GET",
			path:   "/Audio/item1/stream.mp3",
			want:   Decision{Ownership: MediaProxy, Operation: OperationMediaProxy, MethodAllowed: true, Allow: "GET, HEAD"},
		},
		{
			name:   "download admitted",
			method: "GET",
			path:   "/Items/x/Download",
			want:   Decision{Ownership: MediaProxy, Operation: OperationMediaProxy, MethodAllowed: true, Allow: "GET, HEAD"},
		},
		{
			name:   "unknown unclassified",
			method: "GET",
			path:   "/unknown/path",
			want:   Decision{Ownership: Unclassified, Operation: OperationUnclassified, MethodAllowed: false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDecision(t, Classify(tc.method, tc.path), tc.want)
		})
	}
}

func TestPhase5MethodMatrix(t *testing.T) {
	methods := []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}
	cases := []struct {
		name      string
		path      string
		ownership Ownership
		operation Operation
		allow     string
		allowed   map[string]bool
	}{
		{"playback info", "/Items/item/PlaybackInfo", MediaProxy, OperationPlaybackInfo, "GET, POST", map[string]bool{"GET": true, "POST": true}},
		{"live stream open", "/LiveStreams/Open", MediaProxy, OperationLiveStreamOpen, "POST", map[string]bool{"POST": true}},
		{"live stream media info", "/LiveStreams/MediaInfo", MediaProxy, OperationLiveStreamMediaInfo, "POST", map[string]bool{"POST": true}},
		{"live stream close", "/LiveStreams/Close", MediaProxy, OperationLiveStreamClose, "POST", map[string]bool{"POST": true}},
		{"active encodings delete", "/Videos/ActiveEncodings", MediaProxy, OperationActiveEncodingsDelete, "DELETE", map[string]bool{"DELETE": true}},
		{"active encodings delete compat", "/Videos/ActiveEncodings/Delete", MediaProxy, OperationActiveEncodingsDeleteCompat, "POST", map[string]bool{"POST": true}},
		{"websocket", "/embywebsocket", LocalSession, OperationWebSocket, "GET", map[string]bool{"GET": true}},
		{"general command", "/Sessions/public-id/Command", LocalSession, OperationSessionGeneralCommand, "POST", map[string]bool{"POST": true}},
		{"named general command", "/Sessions/public-id/Command/DisplayContent", LocalSession, OperationSessionGeneralCommand, "POST", map[string]bool{"POST": true}},
		{"play command", "/Sessions/public-id/Playing", LocalSession, OperationSessionPlay, "POST", map[string]bool{"POST": true}},
		{"playstate command", "/Sessions/public-id/Playing/Pause", LocalSession, OperationSessionPlaystate, "POST", map[string]bool{"POST": true}},
		{"curated user items", "/Users/u/Items", MetadataProxy, OperationMetadataProxy, "GET, HEAD", map[string]bool{"GET": true, "HEAD": true}},
		{"curated binary image", "/Items/item/Images/Primary", MediaProxy, OperationMediaProxy, "GET, HEAD", map[string]bool{"GET": true, "HEAD": true}},
		{"video stream", "/Videos/item/stream", MediaProxy, OperationMediaProxy, "GET, HEAD", map[string]bool{"GET": true, "HEAD": true}},
		{"video stream container", "/Videos/item/stream.mp4", MediaProxy, OperationMediaProxy, "GET, HEAD", map[string]bool{"GET": true, "HEAD": true}},
		{"video master m3u8", "/Videos/item/master.m3u8", MediaProxy, OperationMediaProxy, "GET, HEAD", map[string]bool{"GET": true, "HEAD": true}},
		{"video main m3u8", "/Videos/item/main.m3u8", MediaProxy, OperationMediaProxy, "GET, HEAD", map[string]bool{"GET": true, "HEAD": true}},
		{"video hls segment", "/Videos/item/hls/seg0.ts", MediaProxy, OperationMediaProxy, "GET, HEAD", map[string]bool{"GET": true, "HEAD": true}},
		{"video hls playlist segment", "/Videos/item/hls/pl/seg0.ts", MediaProxy, OperationMediaProxy, "GET, HEAD", map[string]bool{"GET": true, "HEAD": true}},
		{"download", "/Items/item/Download", MediaProxy, OperationMediaProxy, "GET, HEAD", map[string]bool{"GET": true, "HEAD": true}},
		{"audio stream", "/Audio/item/stream", MediaProxy, OperationMediaProxy, "GET, HEAD", map[string]bool{"GET": true, "HEAD": true}},
		{"video subtitle stream", "/Videos/item/ms/Subtitles/0/Stream.vtt", MediaProxy, OperationMediaProxy, "GET, HEAD", map[string]bool{"GET": true, "HEAD": true}},
		{"items subtitle with ticks", "/Items/item/ms/Subtitles/1/1000/Stream.srt", MediaProxy, OperationMediaProxy, "GET, HEAD", map[string]bool{"GET": true, "HEAD": true}},
	}

	for _, tc := range cases {
		for _, method := range methods {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				want := Decision{
					Ownership:     tc.ownership,
					Operation:     tc.operation,
					MethodAllowed: tc.allowed[method],
					Allow:         tc.allow,
				}
				assertDecision(t, Classify(method, tc.path), want)
			})
		}
	}
}

func TestPhase5NegotiationExactAndNearMisses(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		want   Decision
	}{
		{"playback info mixed case", "post", "/iTeMs/ITEM/pLaYbAcKiNfO/", Decision{MediaProxy, OperationPlaybackInfo, true, "GET, POST"}},
		{"playback info missing id", "POST", "/Items/PlaybackInfo", Decision{Unclassified, OperationUnclassified, false, ""}},
		{"playback info descendant", "GET", "/Items/item/PlaybackInfo/Extra", Decision{Unclassified, OperationUnclassified, false, ""}},
		{"playback info descendant post", "POST", "/Items/item/PlaybackInfo/Extra", Decision{Unclassified, OperationUnclassified, false, ""}},
		{"live stream open descendant", "GET", "/LiveStreams/Open/Extra", Decision{Unclassified, OperationUnclassified, false, ""}},
		{"live stream open lookalike", "POST", "/LiveStreams/Opened", Decision{Unclassified, OperationUnclassified, false, ""}},
		{"live stream media info descendant", "POST", "/LiveStreams/MediaInfo/Extra", Decision{Unclassified, OperationUnclassified, false, ""}},
		{"live stream close lookalike", "HEAD", "/LiveStreams/Closed", Decision{Unclassified, OperationUnclassified, false, ""}},
		{"active encodings descendant", "GET", "/Videos/ActiveEncodings/Extra", Decision{Unclassified, OperationUnclassified, false, ""}},
		{"active encodings descendant delete", "DELETE", "/Videos/ActiveEncodings/Extra", Decision{Unclassified, OperationUnclassified, false, ""}},
		{"active encodings delete descendant", "POST", "/Videos/ActiveEncodings/Delete/Extra", Decision{Unclassified, OperationUnclassified, false, ""}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDecision(t, Classify(tc.method, tc.path), tc.want)
		})
	}
}

func TestPhase5ImageClassification(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		want   Decision
	}{
		{"item images list denied", "GET", "/Items/item/Images", Decision{Unclassified, OperationUnclassified, false, ""}},
		{"item image binary", "HEAD", "/Items/item/Images/Primary", Decision{MediaProxy, OperationMediaProxy, true, "GET, HEAD"}},
		{"item image with decimal index", "GET", "/Items/item/Images/Primary/0", Decision{MediaProxy, OperationMediaProxy, true, "GET, HEAD"}},
		{"item image non-decimal index denied", "GET", "/Items/item/Images/Primary/extra", Decision{Unclassified, OperationUnclassified, false, ""}},
		{"item image wrong method", "POST", "/Items/item/Images/Primary", Decision{MediaProxy, OperationMediaProxy, false, "GET, HEAD"}},
		{"user images root denied", "GET", "/Users/user/Images", Decision{Unclassified, OperationUnclassified, false, ""}},
		{"user image denied", "HEAD", "/Users/user/Images/Primary", Decision{Unclassified, OperationUnclassified, false, ""}},
		{"user image near miss", "GET", "/Users/user/Image/Primary", Decision{Unclassified, OperationUnclassified, false, ""}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDecision(t, Classify(tc.method, tc.path), tc.want)
		})
	}
}

func TestPhase5WebSocketAndCommandBoundaries(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		want   Decision
	}{
		{"websocket case and trailing slash", "GET", "/EmbyWebSocket/", Decision{LocalSession, OperationWebSocket, true, "GET"}},
		{"websocket descendant", "GET", "/embywebsocket/extra", Decision{Unclassified, OperationUnclassified, false, ""}},
		{"websocket lookalike", "GET", "/embywebsockets", Decision{Unclassified, OperationUnclassified, false, ""}},
		{"command extra descendant denied", "POST", "/Sessions/public-id/Command/Name/Extra", Decision{DeniedSession, OperationDeniedSession, true, ""}},
		{"playing extra descendant denied", "POST", "/Sessions/public-id/Playing/Pause/Extra", Decision{DeniedSession, OperationDeniedSession, true, ""}},
		{"system remains denied", "POST", "/Sessions/public-id/System/DisplayContent", Decision{DeniedSession, OperationDeniedSession, true, "POST"}},
		{"queue remains denied", "GET", "/Sessions/public-id/PlayQueue", Decision{DeniedSession, OperationDeniedSession, true, ""}},
		{"viewing remains denied", "POST", "/Sessions/public-id/Viewing", Decision{DeniedSession, OperationDeniedSession, true, "POST"}},
		{"unknown descendant remains denied", "PATCH", "/Sessions/public-id/Unknown/Thing", Decision{DeniedSession, OperationDeniedSession, true, ""}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDecision(t, Classify(tc.method, tc.path), tc.want)
		})
	}
}

func TestPhase8UnknownDefaultIsUnclassified(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		t.Run(method, func(t *testing.T) {
			assertDecision(t, Classify(method, "/Unknown/Path"), Decision{
				Ownership:     Unclassified,
				Operation:     OperationUnclassified,
				MethodAllowed: false,
			})
		})
	}
}

func TestPhase8NoLegacyDefault(t *testing.T) {
	// Unclassified is the only unmatched default; no Legacy ownership remains.
	paths := []string{
		"/Unknown/Path",
		"/Plugins",
		"/Packages",
		"/Devices",
		"/ScheduledTasks",
		"/Items",
		"/Items/item",
		"/Videos/item/original.mkv", // generic StreamFileName denied
		"/Videos/item/file",
		"/Hls/master.m3u8", // top-level Hls family not admitted
		"/Users/u/HideFromResume/i",
		"/Items/item/Images",
		"/Users/u/Suggestions",
		"/System/Configuration",
		"/Videos/item/hls/seg0.xyz", // non-allowlisted segment container
		"/Videos/item/stream.unknowncontainer",
		"/Audio/item/master.m3u8", // audio m3u8 not in W1 executable set
	}
	for _, path := range paths {
		d := Classify("GET", path)
		if d.Ownership != Unclassified || d.Operation != OperationUnclassified || d.MethodAllowed {
			t.Fatalf("path %q want Unclassified denied, got %+v", path, d)
		}
	}
}

func TestPhase8CuratedSupportedTemplates(t *testing.T) {
	// Hard-coded mirror of the frozen curated 20-route set (no runtime JSON parse).
	cases := []struct {
		method string
		path   string
		want   Decision
	}{
		{"GET", "/Branding/Configuration", Decision{LocalPublic, OperationBrandingConfiguration, true, "GET"}},
		{"GET", "/DisplayPreferences/usersettings", Decision{LocalPersonal, OperationPersonal, true, "GET, POST"}},
		{"GET", "/Items/item/Images/Primary", Decision{MediaProxy, OperationMediaProxy, true, "GET, HEAD"}},
		{"HEAD", "/Items/item/Images/Primary", Decision{MediaProxy, OperationMediaProxy, true, "GET, HEAD"}},
		{"GET", "/Items/item/Similar", Decision{MetadataProxy, OperationMetadataProxy, true, "GET, HEAD"}},
		{"GET", "/Items/item/ThemeMedia", Decision{MetadataProxy, OperationMetadataProxy, true, "GET, HEAD"}},
		{"GET", "/System/Endpoint", Decision{MetadataProxy, OperationMetadataProxy, true, "GET, HEAD"}},
		{"GET", "/System/Info", Decision{MetadataProxy, OperationMetadataProxy, true, "GET, HEAD"}},
		{"GET", "/System/Info/Public", Decision{LocalPublic, OperationPublicSystemInfo, true, "GET"}},
		{"GET", "/Users/Public", Decision{LocalPublic, OperationPublicUsers, true, "GET"}},
		{"GET", "/Users/phase8-evidence-user", Decision{LocalPersonal, OperationCurrentUser, true, "GET"}},
		{"GET", "/Users/phase8-evidence-user/Items", Decision{MetadataProxy, OperationMetadataProxy, true, "GET, HEAD"}},
		{"GET", "/Users/phase8-evidence-user/Items/Latest", Decision{LocalPersonal, OperationPersonal, true, "GET"}},
		{"GET", "/Users/phase8-evidence-user/Items/Resume", Decision{LocalPersonal, OperationPersonal, true, "GET"}},
		{"GET", "/Users/phase8-evidence-user/Items/34736", Decision{MetadataProxy, OperationMetadataProxy, true, "GET, HEAD"}},
		{"GET", "/Users/phase8-evidence-user/Items/34736/SpecialFeatures", Decision{MetadataProxy, OperationMetadataProxy, true, "GET, HEAD"}},
		{"GET", "/Users/phase8-evidence-user/Views", Decision{MetadataProxy, OperationMetadataProxy, true, "GET, HEAD"}},
		{"POST", "/Items/34736/PlaybackInfo", Decision{MediaProxy, OperationPlaybackInfo, true, "GET, POST"}},
		{"POST", "/Sessions/Capabilities/Full", Decision{LocalSession, OperationCapabilities, true, "POST"}},
		{"POST", "/Sessions/Logout", Decision{LocalSession, OperationLogout, true, "POST"}},
		{"POST", "/Users/AuthenticateByName", Decision{LocalPublic, OperationAuthenticate, true, "POST"}},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			assertDecision(t, Classify(tc.method, tc.path), tc.want)
		})
	}
}

func TestPhase8UnknownKnownPrefixDescendants(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"users suggestions", "GET", "/Users/u/Suggestions"},
		{"users sections", "GET", "/Users/u/Sections/s/Items"},
		{"users items root extra", "GET", "/Users/u/Items/Root/Extra"},
		{"users items intros", "GET", "/Users/u/Items/i/Intros"},
		{"items alone", "GET", "/Items"},
		{"items detail alone", "GET", "/Items/i"},
		{"items images list", "GET", "/Items/i/Images"},
		{"items images non decimal", "GET", "/Items/i/Images/Primary/extra"},
		{"items images deep", "GET", "/Items/i/Images/Primary/0/Tag"},
		{"items similar descendant", "GET", "/Items/i/Similar/Extra"},
		{"system configuration", "GET", "/System/Configuration"},
		{"system info extra", "GET", "/System/Info/Extra"},
		{"scheduled tasks", "GET", "/ScheduledTasks"},
		{"plugins", "GET", "/Plugins"},
		{"packages", "GET", "/Packages"},
		{"devices", "GET", "/Devices"},
		{"videos generic filename", "GET", "/Videos/i/original.mkv"},
		{"videos bare id", "GET", "/Videos/i"},
		{"videos stream descendant", "GET", "/Videos/i/stream/extra"},
		{"videos hls deep", "GET", "/Videos/i/hls/a/b/c.ts"},
		{"top level hls", "GET", "/Hls/master.m3u8"},
		{"audio master not admitted", "GET", "/Audio/i/master.m3u8"},
		{"subtitle missing mediasource shape", "GET", "/Videos/i/Subtitles/0/Stream.vtt"},
		{"subtitle non numeric index", "GET", "/Videos/i/ms/Subtitles/x/Stream.vtt"},
		{"subtitle bad format", "GET", "/Videos/i/ms/Subtitles/0/Stream.exe"},
		{"hide from resume", "POST", "/Users/u/HideFromResume/i"},
		{"branding extra", "GET", "/Branding/Configuration/Extra"},
		{"views descendant", "GET", "/Users/u/Views/Extra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDecision(t, Classify(tc.method, tc.path), Decision{
				Ownership:     Unclassified,
				Operation:     OperationUnclassified,
				MethodAllowed: false,
			})
		})
	}
}

func TestPhase8MethodDenialOnSupportedTemplates(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		want   Decision
	}{
		{"system info post", "POST", "/System/Info", Decision{MetadataProxy, OperationMetadataProxy, false, "GET, HEAD"}},
		{"user items post", "POST", "/Users/u/Items", Decision{MetadataProxy, OperationMetadataProxy, false, "GET, HEAD"}},
		{"image post", "POST", "/Items/i/Images/Primary", Decision{MediaProxy, OperationMediaProxy, false, "GET, HEAD"}},
		{"video stream post", "POST", "/Videos/i/stream", Decision{MediaProxy, OperationMediaProxy, false, "GET, HEAD"}},
		{"download delete", "DELETE", "/Items/i/Download", Decision{MediaProxy, OperationMediaProxy, false, "GET, HEAD"}},
		{"similar delete", "DELETE", "/Items/i/Similar", Decision{MetadataProxy, OperationMetadataProxy, false, "GET, HEAD"}},
		{"views put", "PUT", "/Users/u/Views", Decision{MetadataProxy, OperationMetadataProxy, false, "GET, HEAD"}},
		{"authenticate get", "GET", "/Users/AuthenticateByName", Decision{LocalPublic, OperationAuthenticate, false, "POST"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDecision(t, Classify(tc.method, tc.path), tc.want)
		})
	}
}

func TestPhase8W1ExactMediaTemplates(t *testing.T) {
	mediaOK := Decision{MediaProxy, OperationMediaProxy, true, "GET, HEAD"}
	mediaDenyMethod := Decision{MediaProxy, OperationMediaProxy, false, "GET, HEAD"}
	unclass := Decision{Unclassified, OperationUnclassified, false, ""}
	cases := []struct {
		name   string
		method string
		path   string
		want   Decision
	}{
		{"download get", "GET", "/Items/item/Download", mediaOK},
		{"download head", "HEAD", "/Items/item/Download", mediaOK},
		{"download post", "POST", "/Items/item/Download", mediaDenyMethod},
		{"videos stream", "GET", "/Videos/item/stream", mediaOK},
		{"videos stream.ts", "GET", "/Videos/item/stream.ts", mediaOK},
		{"videos stream.mp4", "HEAD", "/Videos/item/stream.mp4", mediaOK},
		{"videos master", "GET", "/Videos/item/master.m3u8", mediaOK},
		{"videos main", "GET", "/Videos/item/main.m3u8", mediaOK},
		{"videos hls 4seg", "GET", "/Videos/item/hls/seg0.ts", mediaOK},
		{"videos hls 5seg", "GET", "/Videos/item/hls/pl1/seg0.m4s", mediaOK},
		{"videos hls1 4seg", "GET", "/Videos/item/hls1/seg0.ts", mediaOK},
		{"videos hls1 5seg", "GET", "/Videos/item/hls1/pl/seg1.ts", mediaOK},
		{"videos subtitle", "GET", "/Videos/item/src/Subtitles/0/Stream.vtt", mediaOK},
		{"videos subtitle ticks", "GET", "/Videos/item/src/Subtitles/2/12345/Stream.srt", mediaOK},
		{"items subtitle", "GET", "/Items/item/src/Subtitles/0/Stream.vtt", mediaOK},
		{"items subtitle ticks", "HEAD", "/Items/item/src/Subtitles/1/0/Stream.ass", mediaOK},
		{"audio stream", "GET", "/Audio/item/stream", mediaOK},
		{"audio stream.mp3", "GET", "/Audio/item/stream.mp3", mediaOK},
		// Near misses
		{"generic mkv filename", "GET", "/Videos/item/original.mkv", unclass},
		{"stream extra segment", "GET", "/Videos/item/stream/extra", unclass},
		{"live.m3u8 not admitted", "GET", "/Videos/item/live.m3u8", unclass},
		{"hls bad ext", "GET", "/Videos/item/hls/seg0.bin", unclass},
		{"hls too deep", "GET", "/Videos/item/hls/a/b/c.ts", unclass},
		{"subtitle nondecimal index", "GET", "/Videos/item/src/Subtitles/x/Stream.vtt", unclass},
		{"subtitle bad format", "GET", "/Videos/item/src/Subtitles/0/Stream.exe", unclass},
		{"subtitle missing stream prefix", "GET", "/Videos/item/src/Subtitles/0/vtt", unclass},
		{"audio universal not admitted", "GET", "/Audio/item/universal", unclass},
		{"images list still denied", "GET", "/Items/item/Images", unclass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDecision(t, Classify(tc.method, tc.path), tc.want)
		})
	}
}

func TestClassifyDoesNotEmbedRawUserIDs(t *testing.T) {
	// Decision outputs are enums/flags only; ensure path ids never leak into Allow.
	d := Classify("POST", "/Sessions/real-session-id-xyz/Users/real-user-id-abc")
	if d.Allow != "POST, DELETE" {
		t.Fatalf("Allow=%q", d.Allow)
	}
	if d.Ownership != DeniedSession || d.Operation != OperationDeniedSession {
		t.Fatalf("unexpected decision: %+v", d)
	}
}

func assertDecision(t *testing.T, got, want Decision) {
	t.Helper()
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
