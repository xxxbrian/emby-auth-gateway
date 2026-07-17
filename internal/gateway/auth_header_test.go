package gateway

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractCredentialPrecedence(t *testing.T) {
	shaped, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}

	for _, tc := range []struct {
		name      string
		header    http.Header
		rawQuery  string
		wantToken string
		wantSrc   TokenSource
		wantKey   string
	}{
		{
			name:      "token header beats everything",
			header:    headerPairs("X-Emby-Token", "header-token", "X-Emby-Authorization", `Emby Token="auth-token"`),
			rawQuery:  "api_key=query-token&token=generic-token",
			wantToken: "header-token",
			wantSrc:   TokenSourceTokenHeader,
		},
		{
			name:      "media browser token header",
			header:    headerPairs("X-MediaBrowser-Token", "media-token"),
			wantToken: "media-token",
			wantSrc:   TokenSourceTokenHeader,
		},
		{
			name:      "auth header beats query",
			header:    headerPairs("X-Emby-Authorization", `Emby Token="auth-token"`),
			rawQuery:  "api_key=query-token",
			wantToken: "auth-token",
			wantSrc:   TokenSourceAuthHeader,
		},
		{
			name:      "authorization header token",
			header:    headerPairs("Authorization", `MediaBrowser Token="auth-token"`),
			wantToken: "auth-token",
			wantSrc:   TokenSourceAuthHeader,
		},
		{
			name:      "strict api_key before access_token",
			rawQuery:  "access_token=access&api_key=api&token=generic",
			wantToken: "api",
			wantSrc:   TokenSourceStrictQuery,
			wantKey:   "api_key",
		},
		{
			name:      "strict access_token before X-Emby-Token",
			rawQuery:  "X-Emby-Token=official&access_token=access&token=generic",
			wantToken: "access",
			wantSrc:   TokenSourceStrictQuery,
			wantKey:   "access_token",
		},
		{
			name:      "official query X-Emby-Token before generic token",
			rawQuery:  "token=generic&X-Emby-Token=official",
			wantToken: "official",
			wantSrc:   TokenSourceStrictQuery,
			wantKey:   "X-Emby-Token",
		},
		{
			name:      "generic token last",
			rawQuery:  "token=" + shaped,
			wantToken: shaped,
			wantSrc:   TokenSourceGenericQuery,
			wantKey:   "token",
		},
		{
			name:      "empty higher priority does not block lower",
			header:    headerPairs("X-Emby-Token", "   "),
			rawQuery:  "api_key=&api_key=  &token=generic-token",
			wantToken: "generic-token",
			wantSrc:   TokenSourceGenericQuery,
			wantKey:   "token",
		},
		{
			name:      "first non-empty value within key",
			rawQuery:  "api_key=&api_key=first&api_key=second",
			wantToken: "first",
			wantSrc:   TokenSourceStrictQuery,
			wantKey:   "api_key",
		},
		{
			name:      "query key case sensitive",
			rawQuery:  "API_KEY=upper&api_key=lower",
			wantToken: "lower",
			wantSrc:   TokenSourceStrictQuery,
			wantKey:   "api_key",
		},
		{
			name:      "invalid high priority does not fall back",
			header:    headerPairs("X-Emby-Token", "invalid-high"),
			rawQuery:  "api_key=valid-looking",
			wantToken: "invalid-high",
			wantSrc:   TokenSourceTokenHeader,
		},
		{
			name:      "invalid strict query does not fall back to generic",
			rawQuery:  "api_key=invalid-high&token=generic-token",
			wantToken: "invalid-high",
			wantSrc:   TokenSourceStrictQuery,
			wantKey:   "api_key",
		},
		{
			name: "repeated X-Emby-Token uses first non-empty",
			header: func() http.Header {
				h := make(http.Header)
				h.Add("X-Emby-Token", "   ")
				h.Add("X-Emby-Token", "first-header")
				h.Add("X-Emby-Token", "second-header")
				return h
			}(),
			rawQuery:  "api_key=query-token",
			wantToken: "first-header",
			wantSrc:   TokenSourceTokenHeader,
		},
		{
			name: "repeated X-MediaBrowser-Token uses first non-empty",
			header: func() http.Header {
				h := make(http.Header)
				h.Add("X-MediaBrowser-Token", "")
				h.Add("X-MediaBrowser-Token", "media-first")
				h.Add("X-MediaBrowser-Token", "media-second")
				return h
			}(),
			wantToken: "media-first",
			wantSrc:   TokenSourceTokenHeader,
		},
		{
			name: "repeated Authorization first nonempty is authoritative no later scan",
			header: func() http.Header {
				h := make(http.Header)
				h.Add("Authorization", "   ")
				h.Add("Authorization", `Emby Token="auth-first"`)
				h.Add("Authorization", `Emby Token="auth-second"`)
				return h
			}(),
			rawQuery:  "api_key=query-token",
			wantToken: "auth-first",
			wantSrc:   TokenSourceAuthHeader,
		},
		{
			name: "repeated Authorization non-Emby first nonempty does not scan later or fall back to query",
			header: func() http.Header {
				h := make(http.Header)
				h.Add("Authorization", "Bearer not-emby")
				h.Add("Authorization", `Emby Token="auth-first"`)
				h.Add("Authorization", `Emby Token="auth-second"`)
				return h
			}(),
			rawQuery:  "api_key=query-token&token=generic-token",
			wantToken: "",
			wantSrc:   TokenSourceNone,
		},
		{
			name: "invalid first repeated token header does not fall back to later header value or query",
			header: func() http.Header {
				h := make(http.Header)
				h.Add("X-Emby-Token", "invalid-high")
				h.Add("X-Emby-Token", "also-present")
				return h
			}(),
			rawQuery:  "api_key=query-token",
			wantToken: "invalid-high",
			wantSrc:   TokenSourceTokenHeader,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/emby", nil)
			if tc.rawQuery != "" {
				req.URL.RawQuery = tc.rawQuery
			}
			if tc.header != nil {
				req.Header = tc.header
			}
			got := ExtractCredential(req)
			if got.Token != tc.wantToken || got.Source != tc.wantSrc || got.QueryKey != tc.wantKey {
				t.Fatalf("ExtractCredential() = {Token:%q Source:%v QueryKey:%q}, want {Token:%q Source:%v QueryKey:%q}",
					got.Token, got.Source, got.QueryKey, tc.wantToken, tc.wantSrc, tc.wantKey)
			}
			if ExtractToken(req) != tc.wantToken {
				t.Fatalf("ExtractToken() = %q, want %q", ExtractToken(req), tc.wantToken)
			}
		})
	}
}

func TestExtractClientIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		header   http.Header
		rawQuery string
		want     ClientIdentity
	}{
		{
			name: "structured authorization only with encoded device",
			header: headerPairs(
				"X-Emby-Authorization",
				`Emby Client="SenPlayer", Device="My%20Phone", DeviceId="dev-1", Version="6.1.3"`,
			),
			want: ClientIdentity{Client: "SenPlayer", Device: "My Phone", DeviceID: "dev-1", Version: "6.1.3"},
		},
		{
			name:     "query only all four exact-case params",
			rawQuery: "X-Emby-Client=QueryClient&X-Emby-Device-Name=QueryDevice&X-Emby-Device-Id=query-dev&X-Emby-Client-Version=2.0",
			want:     ClientIdentity{Client: "QueryClient", Device: "QueryDevice", DeviceID: "query-dev", Version: "2.0"},
		},
		{
			name: "standalone headers only with encoded device name",
			header: headerPairs(
				"X-Emby-Client", "HeaderClient",
				"X-Emby-Device-Name", "My%20Phone",
				"X-Emby-Device-Id", "header-dev",
				"X-Emby-Client-Version", "3.0",
			),
			want: ClientIdentity{Client: "HeaderClient", Device: "My Phone", DeviceID: "header-dev", Version: "3.0"},
		},
		{
			name: "mixed authorization device fields with query client",
			header: headerPairs(
				"Authorization",
				`Emby Device="AuthDevice", DeviceId="auth-dev", Version="4.0"`,
			),
			rawQuery: "X-Emby-Client=QueryClient",
			want:     ClientIdentity{Client: "QueryClient", Device: "AuthDevice", DeviceID: "auth-dev", Version: "4.0"},
		},
		{
			name: "empty request",
			want: ClientIdentity{},
		},
		{
			name: "structured value beats standalone and query",
			header: headerPairs(
				"X-Emby-Authorization",
				`Emby Client="AuthClient", Device="AuthDevice", DeviceId="auth-dev", Version="1.0"`,
				"X-Emby-Client", "HeaderClient",
				"X-Emby-Device-Name", "HeaderDevice",
				"X-Emby-Device-Id", "header-dev",
				"X-Emby-Client-Version", "2.0",
			),
			rawQuery: "X-Emby-Client=QueryClient&X-Emby-Device-Name=QueryDevice&X-Emby-Device-Id=query-dev&X-Emby-Client-Version=3.0",
			want:     ClientIdentity{Client: "AuthClient", Device: "AuthDevice", DeviceID: "auth-dev", Version: "1.0"},
		},
		{
			name: "standalone header beats query",
			header: headerPairs(
				"X-Emby-Client", "HeaderClient",
				"X-Emby-Device-Name", "HeaderDevice",
				"X-Emby-Device-Id", "header-dev",
				"X-Emby-Client-Version", "2.0",
			),
			rawQuery: "X-Emby-Client=QueryClient&X-Emby-Device-Name=QueryDevice&X-Emby-Device-Id=query-dev&X-Emby-Client-Version=3.0",
			want:     ClientIdentity{Client: "HeaderClient", Device: "HeaderDevice", DeviceID: "header-dev", Version: "2.0"},
		},
		{
			name: "x-emby-authorization selection does not merge authorization fields",
			header: headerPairs(
				"X-Emby-Authorization", `Emby Client="OnlyClient"`,
				"Authorization", `Emby Device="AuthDevice", DeviceId="auth-dev", Version="9.0"`,
			),
			want: ClientIdentity{Client: "OnlyClient"},
		},
		{
			name: "repeated header values use first trimmed non-empty",
			header: func() http.Header {
				h := make(http.Header)
				h.Add("X-Emby-Client", "  ")
				h.Add("X-Emby-Client", "FirstClient")
				h.Add("X-Emby-Client", "SecondClient")
				h.Add("X-Emby-Device-Name", "  ")
				h.Add("X-Emby-Device-Name", "FirstDevice")
				h.Add("X-Emby-Device-Id", "first-dev")
				h.Add("X-Emby-Device-Id", "second-dev")
				h.Add("X-Emby-Client-Version", "1.0")
				h.Add("X-Emby-Client-Version", "2.0")
				return h
			}(),
			want: ClientIdentity{Client: "FirstClient", Device: "FirstDevice", DeviceID: "first-dev", Version: "1.0"},
		},
		{
			name: "malformed device escape remains raw",
			header: headerPairs(
				"X-Emby-Authorization",
				`Emby Device="%"`,
			),
			want: ClientIdentity{Device: "%"},
		},
		{
			name: "percent 2B becomes literal plus",
			header: headerPairs(
				"X-Emby-Device-Name", "%2B",
			),
			want: ClientIdentity{Device: "+"},
		},
		{
			name:     "query percent 2520 becomes percent 20 without second decode",
			rawQuery: "X-Emby-Device-Name=%2520",
			want:     ClientIdentity{Device: "%20"},
		},
		{
			name:     "lowercase query keys ignored",
			rawQuery: "x-emby-client=LowerClient&X-Emby-Device-Name=KeepDevice",
			want:     ClientIdentity{Device: "KeepDevice"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/emby", nil)
			if tc.rawQuery != "" {
				req.URL.RawQuery = tc.rawQuery
			}
			if tc.header != nil {
				req.Header = tc.header
			}
			got := ExtractClientIdentity(req)
			if got != tc.want {
				t.Fatalf("ExtractClientIdentity() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestIsGatewayShapedToken(t *testing.T) {
	token, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}
	if !IsGatewayShapedToken(token) {
		t.Fatalf("canonical token should be gateway-shaped")
	}
	for _, value := range []string{"", "short", "cdn-signature", token + "x", token[:42]} {
		if IsGatewayShapedToken(value) {
			t.Fatalf("value should not be gateway-shaped")
		}
	}

	// Standard alphabet / padding must be rejected.
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = 0xff
	}
	std := base64.StdEncoding.EncodeToString(raw) // contains '+' and/or '/'
	if !strings.ContainsAny(std, "+/") {
		t.Fatalf("test setup expected standard alphabet markers in %q", std)
	}
	if IsGatewayShapedToken(std) {
		t.Fatal("padded standard base64 must not be gateway-shaped")
	}
	if len(std) >= 43 {
		// Even the unpadded standard form uses +/ and must fail decode as rawurl.
		unpaddedStd := strings.TrimRight(std, "=")
		if IsGatewayShapedToken(unpaddedStd) {
			t.Fatal("standard base64 alphabet must not be gateway-shaped")
		}
	}
	padded := base64.URLEncoding.EncodeToString(raw)
	if IsGatewayShapedToken(padded) {
		t.Fatal("padded base64url must not be gateway-shaped")
	}

	// Non-canonical trailing bits: decode may succeed but re-encode differs.
	canonical := base64.RawURLEncoding.EncodeToString(raw)
	if !IsGatewayShapedToken(canonical) {
		t.Fatal("canonical rawurl token rejected")
	}
	// Flip the last character within the alphabet while keeping length 43.
	// Many such flips still decode; only re-encode equality is authoritative.
	foundNonCanonical := false
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	for i := 0; i < len(alphabet); i++ {
		if alphabet[i] == canonical[42] {
			continue
		}
		candidate := canonical[:42] + string(alphabet[i])
		decoded, err := base64.RawURLEncoding.DecodeString(candidate)
		if err != nil || len(decoded) != 32 {
			continue
		}
		if base64.RawURLEncoding.EncodeToString(decoded) == candidate {
			continue
		}
		foundNonCanonical = true
		if IsGatewayShapedToken(candidate) {
			t.Fatalf("non-canonical trailing-bit token accepted: %q", candidate)
		}
		break
	}
	if !foundNonCanonical {
		t.Fatal("failed to construct a decodable non-canonical token")
	}
}

func headerPairs(kv ...string) http.Header {
	h := make(http.Header)
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}
