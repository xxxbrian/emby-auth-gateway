package gateway

import (
	"net/http"
	"strings"
	"testing"
)

func TestRewriteMediaReference(t *testing.T) {
	session := testSession()
	const gateway = "gateway-token"
	public := "https://media.example.com/emby"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"backend default port", "https://backend.example.com/emby/Videos/a/stream?api_key=backend-token#part", "/Videos/a/stream?api_key=gateway-token#part"},
		{"alias root opaque token rejected", "https://cdn.example.com/Videos/a/stream?x=backend-token", ""},
		{"alias mediabrowser", "https://cdn.example.com/mediabrowser/Audio/a/stream", "/Audio/a/stream?api_key=gateway-token"},
		{"gateway idempotent", "https://media.example.com/emby/Videos/a/stream?api_key=backend-token", "/Videos/a/stream?api_key=gateway-token"},
		{"relative owned", "/Videos/a/stream?api_key=backend-token", "/Videos/a/stream?api_key=gateway-token"},
		{"base relative owned", "/emby/Videos/a/stream?api_key=backend-token", "/Videos/a/stream?api_key=gateway-token"},
		{"mixed case base", "/EmBy/Videos/a/stream", "/Videos/a/stream?api_key=gateway-token"},
		{"external token rejected", "https://outside.example.com/file.mp4?api_key=backend-token&x=keep", ""},
		{"userinfo", "https://user@backend.example.com/emby/Videos/a/stream", "https://user@backend.example.com/emby/Videos/a/stream"},
		{"prefix confusion", "https://backend.example.com/embyfoo/Videos/a/stream", "https://backend.example.com/embyfoo/Videos/a/stream"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := rewriteMediaReference(tt.in, session, testResponseUpstreamSnapshot("https://backend.example.com:443/emby/"), gateway, public, "gateway-server", false); got != tt.want {
				t.Fatalf("rewriteMediaReference() = %q, want %q", got, tt.want)
			}
		})
	}

	absolute := rewriteMediaReference("https://backend.example.com/emby/Videos/a/stream?api_key=backend-token", session, testResponseUpstreamSnapshot("https://backend.example.com:443/emby/"), gateway, public, "gateway-server", true)
	if absolute != "https://media.example.com/emby/Videos/a/stream?api_key=gateway-token" {
		t.Fatalf("absolute = %q", absolute)
	}
	rewritten := rewriteJSONValueWithSnapshot(map[string]any{"DirectStreamUrl": "https://outside.example.com/file?api_key=backend-token"}, session, testResponseUpstreamSnapshot("https://backend.example.com:443/emby/"), gateway, public, "gateway-server").(map[string]any)
	if got := rewritten["DirectStreamUrl"]; got != "" {
		t.Fatalf("external media field = %q", got)
	}
}

func TestResponseURLRewritesUseCapturedSnapshotAfterRuntimeChange(t *testing.T) {
	session := testSession()
	upstream := testResponseUpstreamSnapshot("https://backend.example.com/emby")
	changedRuntime := testResponseUpstreamSnapshot("https://mutated.example.com/other")

	const gatewayToken = "gateway-token"
	const publicBase = "https://gateway.example.com/emby"
	input := "https://backend.example.com/emby/Videos/backend-user/stream?api_key=backend-token&UserId=backend-user&ServerId=backend-server"
	want := "/Videos/backend-user/stream?UserId=gateway-user&ServerId=gateway-server&api_key=gateway-token"
	if got := rewriteMediaReference(input, session, upstream, gatewayToken, publicBase, "gateway-server", false); got != want {
		t.Fatalf("media rewrite = %q, want %q", got, want)
	}
	if got := rewriteM3U8Reference(input, "/Videos/a/master.m3u8", session, upstream, gatewayToken, publicBase, "gateway-server"); got != publicBase+want {
		t.Fatalf("m3u8 rewrite = %q", got)
	}
	if got := rewriteResponseLocation(input, "/Items/a", session, upstream, gatewayToken, publicBase, "gateway-server"); got != publicBase+want {
		t.Fatalf("location rewrite = %q", got)
	}
	if got := rewriteStringWithSnapshot("backend-token backend-user backend-server", session, upstream, gatewayToken, publicBase, "gateway-server"); got != "gateway-token gateway-user gateway-server" {
		t.Fatalf("scalar rewrite = %q", got)
	}
	if strings.Contains(strings.Join([]string{rewriteMediaReference(input, session, upstream, gatewayToken, publicBase, "gateway-server", false), rewriteM3U8Reference(input, "/Videos/a/master.m3u8", session, upstream, gatewayToken, publicBase, "gateway-server"), rewriteResponseLocation(input, "/Items/a", session, upstream, gatewayToken, publicBase, "gateway-server")}, " "), changedRuntime.baseURL) {
		t.Fatal("response rewrite used a later runtime")
	}
}

func TestRewriteM3U8MediaReferences(t *testing.T) {
	session := testSession()
	input := "seg.ts?api_key=backend-token\r\n/Videos/a/root.ts?api_key=backend-token\r\n/emby/Videos/a/base.ts?api_key=backend-token\r\n#EXT-X-KEY:METHOD=AES-128,URI=\"keys/key?api_key=backend-token\"\r\n"
	got := string(rewriteM3U8MediaReferences([]byte(input), "/Videos/a/master.m3u8", session, testResponseUpstreamSnapshot("https://backend.example.com/emby"), "gateway-token", "https://media.example.com/emby", "gateway-server"))
	want := "https://media.example.com/emby/Videos/a/seg.ts?api_key=gateway-token\r\nhttps://media.example.com/emby/Videos/a/root.ts?api_key=gateway-token\r\nhttps://media.example.com/emby/Videos/a/base.ts?api_key=gateway-token\r\n#EXT-X-KEY:METHOD=AES-128,URI=\"https://media.example.com/emby/Videos/a/keys/key?api_key=gateway-token\"\r\n"
	if got != want {
		t.Fatalf("playlist = %q, want %q", got, want)
	}
}

func TestRewriteResponseLocation(t *testing.T) {
	session := testSession()
	public := "https://media.example.com/emby"
	cases := []struct{ in, want string }{
		{"https://backend.example.com/emby/Items/a?api_key=backend-token", "https://media.example.com/emby/Items/a"},
		{"/emby/Items/a?api_key=backend-token", "https://media.example.com/emby/Items/a"},
		{"next?api_key=backend-token", "https://media.example.com/emby/Items/next"},
		{"?x=backend-token", ""},
		{"/Users/u", "https://media.example.com/emby/Users/u"},
		{"https://cdn.example.com/emby/Items/a?api_key=backend-token", ""},
	}
	for _, tt := range cases {
		if got := rewriteResponseLocation(tt.in, "/Items/a", session, testResponseUpstreamSnapshot("https://backend.example.com/emby"), "gateway-token", public, "gateway-server"); got != tt.want {
			t.Fatalf("location %q = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCopyResponseHeadersRewritesLocations(t *testing.T) {
	session := testSession()
	dst := make(http.Header)
	copyResponseHeadersWithSnapshot(dst, http.Header{
		"Location":         []string{"https://backend.example.com/emby/Items/a?api_key=backend-token"},
		"Content-Location": []string{"/emby/Items/b?api_key=backend-token"},
	}, "/Items/a", session, testResponseUpstreamSnapshot("https://backend.example.com/emby"), "gateway-token", "https://media.example.com/emby", "gateway-server")
	if got := dst.Get("Location"); got != "https://media.example.com/emby/Items/a" {
		t.Fatalf("Location = %q", got)
	}
	if got := dst.Get("Content-Location"); got != "https://media.example.com/emby/Items/b" {
		t.Fatalf("Content-Location = %q", got)
	}
}

func TestForeignAliasMediaURLComposesOnceWithGatewayBase(t *testing.T) {
	session := testSession()
	value := rewriteJSONValueWithSnapshot(map[string]any{"DirectStreamUrl": "https://cdn.example.com/emby/videos/item/stream?api_key=backend-token"}, session, testResponseUpstreamSnapshot("https://backend.example.com/emby"), "gateway-token", "https://media.example.com/emby", "gateway-server").(map[string]any)
	media := value["DirectStreamUrl"].(string)
	composed := "/emby" + media
	if media != "/videos/item/stream?api_key=gateway-token" || composed != "/emby/videos/item/stream?api_key=gateway-token" || strings.Contains(composed, "/emby/https://") {
		t.Fatalf("media/composed = %q/%q", media, composed)
	}
}

func TestSameOriginMediaBaseVariants(t *testing.T) {
	cases := []struct{ base, in, want string }{
		{"https://backend.example.com", "https://backend.example.com/emby/Videos/a/stream", "/Videos/a/stream?api_key=gateway-token"},
		{"https://backend.example.com/emby", "https://backend.example.com/Videos/a/stream", "/Videos/a/stream?api_key=gateway-token"},
		{"https://backend.example.com/mediabrowser", "https://backend.example.com/EmBy/Videos/a/stream", "/Videos/a/stream?api_key=gateway-token"},
		{"https://backend.example.com/emby", "https://backend.example.com/embyfoo/Videos/a/stream", "https://backend.example.com/embyfoo/Videos/a/stream"},
	}
	for _, tt := range cases {
		session := testSession()
		if got := rewriteMediaReference(tt.in, session, testResponseUpstreamSnapshot(tt.base), "gateway-token", "https://media.example.com/emby", "gateway-server", false); got != tt.want {
			t.Fatalf("base=%q input=%q got=%q want=%q", tt.base, tt.in, got, tt.want)
		}
	}
}

func TestSameOriginVariantsHaveOneGatewayBaseInHLSAndLocations(t *testing.T) {
	for _, backendBase := range []string{"https://backend.example.com", "https://backend.example.com/emby", "https://backend.example.com/mediabrowser"} {
		session := testSession()
		input := "https://backend.example.com/emby/Videos/a/stream?api_key=backend-token"
		want := "https://media.example.com/emby/Videos/a/stream?api_key=gateway-token"
		if got := rewriteM3U8Reference(input, "/Videos/a/master.m3u8", session, testResponseUpstreamSnapshot(backendBase), "gateway-token", "https://media.example.com/emby", "gateway-server"); got != want {
			t.Fatalf("hls base=%q got=%q", backendBase, got)
		}
		if got := rewriteResponseLocation(input, "/Items/a", session, testResponseUpstreamSnapshot(backendBase), "gateway-token", "https://media.example.com/emby", "gateway-server"); got != want {
			t.Fatalf("location base=%q got=%q", backendBase, got)
		}
	}
}

func TestSpecializedReferencesNeverLeakBackendToken(t *testing.T) {
	session := testSession()
	inputs := []string{
		"https://outside.example.com/Videos/a?signature=backend-token",
		"https://outside.example.com/backend-token/Videos/a",
		"https://outside.example.com/Videos/a#backend-token",
		"unrecognized?api_key=backend-token",
		"https://outside.example.com/Videos/a\r\nbackend-token",
	}
	for _, input := range inputs {
		upstream := testResponseUpstreamSnapshot("https://backend.example.com/emby")
		jsonValue := rewriteMediaReference(input, session, upstream, "gateway-token", "https://media.example.com/emby", "gateway-server", false)
		hlsValue := rewriteM3U8Reference(input, "/Videos/a/master.m3u8", session, upstream, "gateway-token", "https://media.example.com/emby", "gateway-server")
		location := rewriteResponseLocation(input, "/Items/a", session, upstream, "gateway-token", "https://media.example.com/emby", "gateway-server")
		for _, output := range []string{jsonValue, hlsValue, location} {
			if strings.Contains(output, upstream.token) {
				t.Fatalf("backend token leaked for %q in %q", input, output)
			}
		}
	}
}

func TestOwnedReferencesRewriteIdentityQueryValues(t *testing.T) {
	session := testSession()
	input := "https://backend.example.com/emby/Videos/a/stream?UserId=backend-user&ServerId=backend-server&api_key=backend-token"
	wantRelative := "/Videos/a/stream?UserId=gateway-user&ServerId=gateway-server&api_key=gateway-token"
	upstream := testResponseUpstreamSnapshot("https://backend.example.com/emby")
	if got := rewriteMediaReference(input, session, upstream, "gateway-token", "https://media.example.com/emby", "gateway-server", false); got != wantRelative {
		t.Fatalf("json owned = %q", got)
	}
	wantAbsolute := "https://media.example.com/emby" + wantRelative
	if got := rewriteM3U8Reference(input, "/Videos/a/master.m3u8", session, upstream, "gateway-token", "https://media.example.com/emby", "gateway-server"); got != wantAbsolute {
		t.Fatalf("hls owned = %q", got)
	}
	if got := rewriteResponseLocation(input, "/Items/a", session, upstream, "gateway-token", "https://media.example.com/emby", "gateway-server"); got != wantAbsolute {
		t.Fatalf("location owned = %q", got)
	}
}

func TestOwnedReferenceOnlyAdaptsSemanticQueryKeys(t *testing.T) {
	session := testSession()
	upstream := testResponseUpstreamSnapshot("https://backend.example.com/emby")
	input := "/Videos/backend-user/stream?sig=a%2Bb&dup=one&dup=two+words&opaque=backend-user&UserId=backend-user&ServerId=backend-server&token=backend-token"
	want := "/Videos/backend-user/stream?sig=a%2Bb&dup=one&dup=two+words&opaque=backend-user&UserId=gateway-user&ServerId=gateway-server&token=gateway-token&api_key=gateway-token"
	if got := rewriteMediaReference(input, session, upstream, "gateway-token", "https://media.example.com/emby", "gateway-server", false); got != want {
		t.Fatalf("owned query=%q, want %q", got, want)
	}
}

func TestConfiguredOriginResolverForNonMediaHLSAndLocations(t *testing.T) {
	cases := []struct{ base, input string }{
		{"https://backend.example.com", "https://backend.example.com/emby/Items/a"},
		{"https://backend.example.com/emby", "https://backend.example.com/Items/a"},
		{"https://backend.example.com/mediabrowser", "https://backend.example.com/mediabrowser/Items/a"},
	}
	for _, tt := range cases {
		session := testSession()
		want := "https://media.example.com/emby/Items/a"
		if got := rewriteM3U8Reference(tt.input, "/Videos/a/master.m3u8", session, testResponseUpstreamSnapshot(tt.base), "gateway-token", "https://media.example.com/emby", "gateway-server"); got != want+"?api_key=gateway-token" {
			t.Fatalf("hls base=%q got=%q", tt.base, got)
		}
		if got := rewriteResponseLocation(tt.input, "/Items/a", session, testResponseUpstreamSnapshot(tt.base), "gateway-token", "https://media.example.com/emby", "gateway-server"); got != want {
			t.Fatalf("location base=%q got=%q", tt.base, got)
		}
	}
	noMatch := "https://backend.example.com/embyfoo/Items/a"
	session := testSession()
	if got := rewriteResponseLocation(noMatch, "/Items/a", session, testResponseUpstreamSnapshot("https://backend.example.com/emby"), "gateway-token", "https://media.example.com/emby", "gateway-server"); got != noMatch {
		t.Fatalf("prefix confusion location = %q", got)
	}
}

func TestSpecializedReferenceDecodedTokenAndNetworkPathSafety(t *testing.T) {
	session := testSession()
	upstream := testResponseUpstreamSnapshot("https://backend.example.com/emby")
	unsafe := []string{
		"https://user:backend-token@outside.example/a",
		"mailto:backend-token@example.com",
		"https://outside.example/%62ackend-token/a",
		"https://outside.example/a?x=%62ackend-token",
		"https://outside.example/a#%62ackend-token",
		"https://outside.example/%zzbackend-token",
		"//outside.example/a?x=backend-token",
	}
	for _, input := range unsafe {
		for _, output := range []string{
			rewriteMediaReference(input, session, upstream, "gateway-token", "https://media.example.com/emby", "gateway-server", false),
			rewriteM3U8Reference(input, "/Videos/a/master.m3u8", session, upstream, "gateway-token", "https://media.example.com/emby", "gateway-server"),
			rewriteResponseLocation(input, "/Items/a", session, upstream, "gateway-token", "https://media.example.com/emby", "gateway-server"),
		} {
			if output != "" {
				t.Fatalf("unsafe %q produced %q", input, output)
			}
		}
	}
	for _, input := range []string{"https://outside.example/a", "//outside.example/a"} {
		if got := rewriteMediaReference(input, session, upstream, "gateway-token", "https://media.example.com/emby", "gateway-server", false); got != input {
			t.Fatalf("harmless JSON external %q became %q", input, got)
		}
		if got := rewriteM3U8Reference(input, "/Videos/a/master.m3u8", session, upstream, "gateway-token", "https://media.example.com/emby", "gateway-server"); got != input {
			t.Fatalf("harmless HLS external %q became %q", input, got)
		}
		if got := rewriteResponseLocation(input, "/Items/a", session, upstream, "gateway-token", "https://media.example.com/emby", "gateway-server"); got != input {
			t.Fatalf("harmless location external %q became %q", input, got)
		}
	}
}

func TestRewriteOwnedQueryDoesNotMatchEmptySources(t *testing.T) {
	session := testSession()
	emptyUpstream := upstreamRequestSnapshot{}
	if got, ok := rewriteOwnedQuery("foo=&UserId=&ServerId=", session, emptyUpstream, "gateway-token", "gateway-server"); !ok || got != "foo=&UserId=&ServerId=" {
		t.Fatalf("empty sources rewrote query to %q", got)
	}
}

func TestOwnedResourceQueriesAreCanonicalizedWithoutReorderingSignedSegments(t *testing.T) {
	session := testSession()
	upstream := testResponseUpstreamSnapshot("https://backend.example.com/emby")
	input := "/Videos/a/stream?sig=a%2Bb&api_key=stale&X-Emby-Token=&x=1&access_token=conflict&token=backend-token#part"
	want := "/Videos/a/stream?sig=a%2Bb&x=1&token=gateway-token&api_key=gateway-token#part"
	if got := rewriteMediaReference(input, session, upstream, "gateway-token", "https://media.example.com/emby", "gateway-server", false); got != want {
		t.Fatalf("canonical resource query = %q, want %q", got, want)
	}
	if got := rewriteMediaReference("/Videos/a/stream?sig=a%2Bb&api_key=stale", session, upstream, "", "https://media.example.com/emby", "gateway-server", false); got != "/Videos/a/stream?sig=a%2Bb" {
		t.Fatalf("cookie resource query = %q", got)
	}
	if got := rewriteMediaReference("/Videos/a/stream?API_KEY=keep&sig=a%2Bb", session, upstream, "gateway-token", "https://media.example.com/emby", "gateway-server", false); got != "/Videos/a/stream?sig=a%2Bb&api_key=gateway-token" {
		t.Fatalf("mixed-case credential alias was not removed: %q", got)
	}
}

func TestMalformedOwnedResourceQueriesFailClosed(t *testing.T) {
	session := testSession()
	upstream := testResponseUpstreamSnapshot("https://backend.example.com/emby")
	for _, output := range []string{
		rewriteMediaReference("/Videos/a/stream?x=%zz", session, upstream, "gateway-token", "https://media.example.com/emby", "gateway-server", false),
		rewriteM3U8Reference("https://backend.example.com/emby/Videos/a/stream?x=%zz", "/Videos/a/master.m3u8", session, upstream, "gateway-token", "https://media.example.com/emby", "gateway-server"),
		rewriteResponseLocation("https://backend.example.com/emby/Videos/a/stream?x=%zz", "/Videos/a/master.m3u8", session, upstream, "gateway-token", "https://media.example.com/emby", "gateway-server"),
	} {
		if output != "" {
			t.Fatalf("malformed owned reference = %q, want empty", output)
		}
	}
}

func TestMalformedPercentEscapeFailsClosedBeforeTokenDecoding(t *testing.T) {
	session := testSession()
	upstream := testResponseUpstreamSnapshot("https://backend.example.com/emby")
	const input = "https://outside.example/a?bad=%zz&x=%62ackend-token"
	const gatewayToken = "gateway-token"
	for _, output := range []string{
		rewriteMediaReference(input, session, upstream, gatewayToken, "https://media.example.com/emby", "gateway-server", false),
		rewriteM3U8Reference(input, "/Videos/a/master.m3u8", session, upstream, gatewayToken, "https://media.example.com/emby", "gateway-server"),
		rewriteResponseLocation(input, "/Items/a", session, upstream, gatewayToken, "https://media.example.com/emby", "gateway-server"),
	} {
		if output != "" || strings.Contains(output, upstream.token) || strings.Contains(output, gatewayToken) {
			t.Fatalf("unsafe reference output = %q", output)
		}
	}
	playlist := string(rewriteM3U8MediaReferences([]byte(`#EXT-X-KEY:METHOD=AES-128,URI="`+input+`"`), "/Videos/a/master.m3u8", session, upstream, gatewayToken, "https://media.example.com/emby", "gateway-server"))
	if strings.Contains(playlist, upstream.token) || strings.Contains(playlist, gatewayToken) || playlist != `#EXT-X-KEY:METHOD=AES-128,URI=""` {
		t.Fatalf("unsafe HLS URI attribute = %q", playlist)
	}
}

func TestCookieResponseRewriteDoesNotExposeGatewayToken(t *testing.T) {
	session := testSession()
	upstream := testResponseUpstreamSnapshot("https://backend.example.com/emby")
	manifest := string(rewriteM3U8MediaReferences([]byte("https://backend.example.com/emby/Videos/a/seg.ts?api_key=backend-token\n#EXT-X-KEY:URI=\"https://backend.example.com/emby/Videos/a/key?api_key=backend-token\""), "/Videos/a/master.m3u8", session, upstream, "", "https://media.example.com/emby", "gateway-server"))
	if strings.Contains(manifest, upstream.token) || strings.Contains(manifest, "gateway-token") || strings.Contains(manifest, "api_key=") {
		t.Fatalf("cookie manifest = %q", manifest)
	}
	location := rewriteResponseLocation("https://backend.example.com/emby/Videos/a/seg.ts?api_key=backend-token", "/Videos/a/master.m3u8", session, upstream, "", "https://media.example.com/emby", "gateway-server")
	if strings.Contains(location, upstream.token) || strings.Contains(location, "gateway-token") || strings.Contains(location, "api_key=") {
		t.Fatalf("cookie location = %q", location)
	}
}

func testResponseUpstreamSnapshot(baseURL string) upstreamRequestSnapshot {
	return upstreamRequestSnapshot{baseURL: baseURL, serverID: "backend-server", userID: "backend-user", token: "backend-token", identity: backendIdentityForTest("backend-device")}
}
