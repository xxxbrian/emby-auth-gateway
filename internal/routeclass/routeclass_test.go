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
			want:   Decision{Ownership: MetadataProxy, Operation: OperationLegacyProxy, MethodAllowed: true},
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
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "targeted playing command",
			method: "POST",
			path:   "/Sessions/sid-1/Playing/Unpause",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "targeted playing wrong method",
			method: "GET",
			path:   "/Sessions/sid-1/Playing/Unpause",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: false, Allow: "POST"},
		},
		{
			name:   "targeted command",
			method: "POST",
			path:   "/Sessions/sid-1/Command",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "targeted command name",
			method: "POST",
			path:   "/Sessions/sid-1/Command/MoveUp",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: true, Allow: "POST"},
		},
		{
			name:   "targeted command wrong method",
			method: "DELETE",
			path:   "/Sessions/sid-1/Command/MoveUp",
			want:   Decision{Ownership: DeniedSession, Operation: OperationDeniedSession, MethodAllowed: false, Allow: "POST"},
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
			// leading spaces prevent leading-slash match of exact public route
			want: Decision{Ownership: LegacyProxy, Operation: OperationLegacyProxy, MethodAllowed: true},
		},
		{
			name:   "trailing space path is not exact public system info",
			method: "GET",
			path:   "/System/Info/Public  ",
			// trailing space breaks exact Public match; remaining /System/Info* is MetadataProxy
			want: Decision{Ownership: MetadataProxy, Operation: OperationLegacyProxy, MethodAllowed: true},
		},
		{
			name:   "sessionsx is not sessions",
			method: "GET",
			path:   "/SessionsX",
			want:   Decision{Ownership: LegacyProxy, Operation: OperationLegacyProxy, MethodAllowed: true},
		},
		{
			name:   "sessionsx descendant is not sessions",
			method: "POST",
			path:   "/SessionsX/Playing",
			want:   Decision{Ownership: LegacyProxy, Operation: OperationLegacyProxy, MethodAllowed: true},
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
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: true},
		},
		{
			name:   "played items write",
			method: "POST",
			path:   "/Users/u1/PlayedItems/item-1",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: true},
		},
		{
			name:   "favorite items write",
			method: "POST",
			path:   "/Users/u1/FavoriteItems/item-1",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: true},
		},
		{
			name:   "rating write",
			method: "POST",
			path:   "/Users/u1/Items/item-1/Rating",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: true},
		},
		{
			name:   "userdata write",
			method: "POST",
			path:   "/Users/u1/Items/item-1/UserData",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: true},
		},
		{
			name:   "resume",
			method: "GET",
			path:   "/Users/u1/Items/Resume",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: true},
		},
		{
			name:   "latest",
			method: "GET",
			path:   "/Users/u1/Items/Latest",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: true},
		},
		{
			name:   "next up",
			method: "GET",
			path:   "/Shows/NextUp",
			want:   Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: true},
		},
		{
			name:   "user items list metadata",
			method: "GET",
			path:   "/Users/u1/Items",
			want:   Decision{Ownership: MetadataProxy, Operation: OperationLegacyProxy, MethodAllowed: true},
		},
		{
			name:   "playback info metadata",
			method: "GET",
			path:   "/Items/abc/PlaybackInfo",
			want:   Decision{Ownership: MetadataProxy, Operation: OperationLegacyProxy, MethodAllowed: true},
		},
		{
			name:   "video stream media",
			method: "GET",
			path:   "/Videos/item1/stream",
			want:   Decision{Ownership: MediaProxy, Operation: OperationLegacyProxy, MethodAllowed: true},
		},
		{
			name:   "audio stream media",
			method: "GET",
			path:   "/Audio/item1/stream.mp3",
			want:   Decision{Ownership: MediaProxy, Operation: OperationLegacyProxy, MethodAllowed: true},
		},
		{
			name:   "download media",
			method: "GET",
			path:   "/Items/x/Download",
			want:   Decision{Ownership: MediaProxy, Operation: OperationLegacyProxy, MethodAllowed: true},
		},
		{
			name:   "unknown legacy",
			method: "GET",
			path:   "/unknown/path",
			want:   Decision{Ownership: LegacyProxy, Operation: OperationLegacyProxy, MethodAllowed: true},
		},
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
