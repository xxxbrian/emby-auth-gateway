package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestNonMediaPurposesRejectEveryRedirect(t *testing.T) {
	next := httptest.NewRequest(http.MethodGet, "https://target.test/item", nil)
	via := []*http.Request{httptest.NewRequest(http.MethodGet, "https://origin.test/item", nil)}
	for _, purpose := range []upstreamPurpose{
		upstreamPurposeMetadata,
		upstreamPurposeNegotiation,
		upstreamPurposeManagedAuth,
		upstreamPurpose(0),
	} {
		if err := upstreamRedirectPolicy(purpose, "gateway", "backend")(next, via); !errors.Is(err, ErrUpstreamRedirectRejected) {
			t.Fatalf("purpose %v redirect error = %v", purpose, err)
		}
	}
}

func TestCloneMediaRedirectRequestSchemesDowngradeAndHopLimit(t *testing.T) {
	tests := []struct {
		name    string
		from    string
		to      string
		hops    int
		wantErr error
	}{
		{"http", "http://origin.test/a", "http://origin.test/b", 1, nil},
		{"https", "https://origin.test/a", "https://cdn.test/b", 1, nil},
		{"upgrade", "http://origin.test/a", "https://origin.test/b", 1, nil},
		{"downgrade", "https://origin.test/a", "http://origin.test/b", 1, ErrMediaRedirectDowngrade},
		{"target ftp", "https://origin.test/a", "ftp://cdn.test/b", 1, ErrMediaRedirectScheme},
		{"source ftp", "ftp://origin.test/a", "https://cdn.test/b", 1, ErrMediaRedirectScheme},
		{"five hops", "https://origin.test/a", "https://cdn.test/b", 5, nil},
		{"six hops", "https://origin.test/a", "https://cdn.test/b", 6, ErrMediaRedirectLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			via := make([]*http.Request, tt.hops)
			for i := range via {
				via[i] = httptest.NewRequest(http.MethodGet, tt.from, nil)
			}
			next := httptest.NewRequest(http.MethodGet, tt.to, nil)
			_, err := CloneMediaRedirectRequest(next, via, "gateway", "backend")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestCloneMediaRedirectRequestCrossOriginStripsCredentialsOnly(t *testing.T) {
	previous := httptest.NewRequest(http.MethodGet, "https://origin.test/video", nil)
	next := httptest.NewRequest(http.MethodGet, "https://cdn.test/video?API_KEY=x&Access_Token=y&x-EMBY-token=z&X-MediaBrowser-Token=q&token=gateway-token&session=backend-token&case=GATEWAY-TOKEN&signature=a%2Bb&Policy=cdn&Key-Pair-Id=k", nil)
	next.Header = http.Header{
		"cOoKiE":                       {"session=gateway-token"},
		"AUTHORIZATION":                {"Emby Token=backend-token"},
		"pRoXy-AuThOrIzAtIoN":          {"Basic secret"},
		"rEfErEr":                      {"https://origin.test"},
		"x-eMbY-aUtHoRiZaTiOn":         {"Emby Token=backend-token"},
		"X-mEdIaBrOwSeR-CuStOm-SeCrEt": {"backend-token"},
		"Range":                        {"bytes=100-"},
		"Accept":                       {"video/*"},
		"If-Range":                     {"etag"},
	}
	originalHeader := next.Header.Clone()
	originalURL := *next.URL

	clone, err := CloneMediaRedirectRequest(next, []*http.Request{previous}, "gateway-token", "backend-token")
	if err != nil {
		t.Fatal(err)
	}
	if clone == next || clone.URL == next.URL || reflect.DeepEqual(clone.Header, next.Header) {
		t.Fatal("redirect request was not isolated before sanitizing")
	}
	if !reflect.DeepEqual(next.Header, originalHeader) || *next.URL != originalURL {
		t.Fatal("input redirect request was mutated")
	}
	for name := range clone.Header {
		if isCrossOriginMediaCredentialHeader(name) {
			t.Fatalf("credential header %q remained", name)
		}
	}
	if clone.Header.Get("Range") != "bytes=100-" || clone.Header.Get("Accept") != "video/*" || clone.Header.Get("If-Range") != "etag" {
		t.Fatalf("media semantics changed: %#v", clone.Header)
	}
	query := clone.URL.Query()
	for _, key := range []string{"API_KEY", "Access_Token", "x-EMBY-token", "X-MediaBrowser-Token", "token", "session"} {
		if _, ok := query[key]; ok {
			t.Fatalf("credential query %q remained: %v", key, query)
		}
	}
	if query.Get("signature") != "a+b" || query.Get("Policy") != "cdn" || query.Get("Key-Pair-Id") != "k" {
		t.Fatalf("CDN signature parameters changed: %v", query)
	}
	if query.Get("case") != "GATEWAY-TOKEN" {
		t.Fatalf("credential values were matched case-insensitively: %v", query)
	}
	if clone.URL.RawQuery != "case=GATEWAY-TOKEN&signature=a%2Bb&Policy=cdn&Key-Pair-Id=k" {
		t.Fatalf("raw CDN query changed: %q", clone.URL.RawQuery)
	}
}

func TestCloneMediaRedirectRequestSameOriginPreservesSemantics(t *testing.T) {
	previous := httptest.NewRequest(http.MethodGet, "HTTPS://EXAMPLE.test:443/video", nil)
	next := httptest.NewRequest(http.MethodGet, "https://example.test/next?api_key=backend&signature=cdn", nil)
	next.Header.Set("Authorization", "credential")
	next.Header.Set("Cookie", "cookie")
	next.Header.Set("Range", "bytes=0-")

	clone, err := CloneMediaRedirectRequest(next, []*http.Request{previous}, "gateway", "backend")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(clone.Header, next.Header) || clone.URL.String() != next.URL.String() {
		t.Fatalf("same-origin semantics changed: %#v %s", clone.Header, clone.URL)
	}
	clone.Header.Set("Range", "bytes=10-")
	clone.URL.RawQuery = "changed=true"
	if next.Header.Get("Range") != "bytes=0-" || next.URL.Query().Get("api_key") != "backend" {
		t.Fatal("same-origin clone aliases its input")
	}
}

func TestMediaRedirectPolicyPurposeCannotComeFromRequest(t *testing.T) {
	previous := httptest.NewRequest(http.MethodGet, "https://origin.test/video", nil)
	next := httptest.NewRequest(http.MethodGet, "https://cdn.test/video?purpose=legacy&api_key=backend", nil)
	next.Header.Set("X-Upstream-Purpose", "metadata")
	if err := upstreamRedirectPolicy(upstreamPurposeMedia, "gateway", "backend")(next, []*http.Request{previous}); err != nil {
		t.Fatal(err)
	}
	if next.URL.Query().Get("purpose") != "legacy" || next.Header.Get("X-Upstream-Purpose") != "metadata" {
		t.Fatal("caller data selected or altered the bound redirect purpose")
	}
	if next.URL.Query().Get("api_key") != "" {
		t.Fatal("bound media policy did not sanitize credentials")
	}
}

func TestStreamingClientsCloneBoundedTransportWithoutTotalTimeout(t *testing.T) {
	baseTransport := &http.Transport{}
	base := &http.Client{Transport: baseTransport, Timeout: 15 * time.Second}
	first := newStreamingHTTPClient(base)
	second := newStreamingHTTPClient(base)
	firstTransport, ok := first.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("first transport=%T", first.Transport)
	}
	secondTransport, ok := second.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("second transport=%T", second.Transport)
	}
	if first.Timeout != 0 || second.Timeout != 0 || firstTransport == baseTransport || secondTransport == baseTransport || firstTransport == secondTransport {
		t.Fatal("streaming clients did not isolate client/transport state")
	}
	if firstTransport.DialContext == nil || firstTransport.TLSHandshakeTimeout != streamTLSHandshakeTimeout || firstTransport.ResponseHeaderTimeout != streamResponseHeaderTimeout || firstTransport.IdleConnTimeout != streamIdleConnectionTimeout {
		t.Fatalf("bounded transport=%#v", firstTransport)
	}
	if base.Timeout != 15*time.Second || baseTransport.DialContext != nil || baseTransport.ResponseHeaderTimeout != 0 {
		t.Fatal("base client or transport was mutated")
	}
}

func isCrossOriginMediaCredentialHeader(name string) bool {
	header := http.Header{name: {"value"}}
	stripCrossOriginMediaHeaders(header)
	return len(header) == 0
}
