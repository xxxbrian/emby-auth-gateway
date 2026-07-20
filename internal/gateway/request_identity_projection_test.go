package gateway

import "testing"

func TestRequestIdentityProjectionOnlyRewritesSemanticUserPathSlot(t *testing.T) {
	for _, tt := range []struct {
		in, want string
	}{
		{"/Users/gateway-user/Items/gateway-user-copy", "/Users/backend-user/Items/gateway-user-copy"},
		{"/Videos/gateway-user/stream", "/Videos/gateway-user/stream"},
		{"/Users/gateway-user-copy/Images/gateway-user", "/Users/gateway-user-copy/Images/gateway-user"},
		{"/uSeRs/gateway-user/Images/Primary", "/uSeRs/backend-user/Images/Primary"},
	} {
		if got := projectUserPath(tt.in, "gateway-user", "backend-user"); got != tt.want {
			t.Fatalf("projectUserPath(%q)=%q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRequestIdentityProjectionPreservesOpaqueQueryPairs(t *testing.T) {
	raw := "sig=a%2Bb&dup=one&dup=two+words&UserId=gateway-user&opaque=backend-user&API_KEY=old&flag"
	got, err := rewriteProxyRawQuery(raw, &Session{SyntheticUserID: "gateway-user"}, upstreamRequestSnapshot{userID: "backend-user", token: "backend-token"})
	if err != nil {
		t.Fatal(err)
	}
	want := "sig=a%2Bb&dup=one&dup=two+words&UserId=backend-user&opaque=backend-user&flag&api_key=backend-token"
	if got != want {
		t.Fatalf("query=%q, want %q", got, want)
	}
}

func TestRequestIdentityProjectionRemovesOnlyExactSelectedCredentialValues(t *testing.T) {
	selected := "selected-gateway-credential"
	raw := "first=one&signature=" + selected + "&signature=prefix-" + selected + "-suffix&signed=a%2Bb&dup=one&dup=two+words&last=two"
	got, err := rewriteProxyRawQuery(raw, &Session{
		GatewayTokenHash: HashToken(selected),
		SyntheticUserID:  "gateway-user",
	}, upstreamRequestSnapshot{userID: "backend-user", token: "backend-token"})
	if err != nil {
		t.Fatal(err)
	}
	want := "first=one&signature=prefix-" + selected + "-suffix&signed=a%2Bb&dup=one&dup=two+words&last=two&api_key=backend-token"
	if got != want {
		t.Fatalf("query=%q, want %q", got, want)
	}
}
