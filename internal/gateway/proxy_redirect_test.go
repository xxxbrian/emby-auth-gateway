package gateway

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
)

func TestProxyClientRedirectSameOriginPreservesHeaders(t *testing.T) {
	server := NewServer(Config{}, NewMemoryStore())
	req := httptest.NewRequest(http.MethodGet, "https://EXAMPLE.test/video", nil)
	req.Header.Set("Authorization", "credential")
	req.Header.Set("X-Emby-Token", "token")
	next := httptest.NewRequest(http.MethodGet, "https://example.test/next", nil)
	next.Header = req.Header.Clone()

	if err := server.proxyClient.CheckRedirect(next, []*http.Request{req}); err != nil {
		t.Fatal("same-origin redirect rejected")
	}
	if next.Header.Get("Authorization") == "" || next.Header.Get("X-Emby-Token") == "" {
		t.Fatal("same-origin redirect stripped credentials")
	}
}

func TestProxyClientRedirectCrossOriginSanitizesHeadersAndTokens(t *testing.T) {
	initial := httptest.NewRequest(http.MethodGet, "https://origin.test/video", nil)
	initial.Header.Set("Authorization", "credential")
	initial.Header.Set("Proxy-Authorization", "credential")
	initial.Header.Set("WWW-Authenticate", "credential")
	initial.Header.Set("Proxy-Authenticate", "credential")
	initial.Header.Set("Cookie", "cookie")
	initial.Header.Set("Cookie2", "cookie")
	initial.Header.Set("Referer", "https://origin.test")
	initial.Header.Set("X-Emby-Token", "header-token")
	initial.Header.Set("X-MediaBrowser-Token", "media-token")
	initial.Header.Set("X-Emby-Authorization", `Emby Token="auth-token"`)
	initial.Header.Set("Range", "bytes=10-")
	initial.Header.Set("If-Range", "etag")
	initial.Header.Set("Accept", "video/*")

	for _, target := range []string{
		"http://origin.test/video",
		"https://origin.test:444/video",
		"https://other.test/video",
	} {
		t.Run(target, func(t *testing.T) {
			next := httptest.NewRequest(http.MethodGet, target+"?token=auth-token&token=keep&api_key=header-token&access_token=other&X-Emby-Token=official&signature=cdn", nil)
			next.Header = initial.Header.Clone()
			if err := newProxyClient(nil).CheckRedirect(next, []*http.Request{initial}); err != nil {
				t.Fatal("cross-origin redirect rejected")
			}
			for name := range next.Header {
				if isCrossOriginSensitiveHeader(name) {
					t.Fatalf("sensitive header %q was forwarded", name)
				}
			}
			if next.Header.Get("Range") == "" || next.Header.Get("If-Range") == "" || next.Header.Get("Accept") == "" {
				t.Fatal("media headers were not preserved")
			}
			query := next.URL.Query()
			if values := query["token"]; len(values) != 1 || values[0] != "keep" {
				t.Fatalf("generic token values = %v", values)
			}
			if query.Get("api_key") != "" || query.Get("access_token") != "" || query.Get("X-Emby-Token") != "" {
				t.Fatalf("strict auth query keys were not stripped: %v", query)
			}
			if query.Get("signature") != "cdn" {
				t.Fatal("signature was not preserved")
			}
		})
	}
}

func TestSanitizeCrossOriginRedirectStripsStrictKeysWithoutTokenMatch(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://target.test?api_key=unrelated&access_token=also&X-Emby-Token=official&token=keep&signature=cdn", nil)
	sanitizeCrossOriginRedirect(req, nil)
	query := req.URL.Query()
	if query.Get("api_key") != "" || query.Get("access_token") != "" || query.Get("X-Emby-Token") != "" {
		t.Fatalf("strict keys remained: %v", query)
	}
	if query.Get("token") != "keep" || query.Get("signature") != "cdn" {
		t.Fatalf("non-sensitive query changed: %v", query)
	}
}

func TestSanitizeCrossOriginRedirectUsesContextGatewayToken(t *testing.T) {
	initial := httptest.NewRequest(http.MethodGet, "https://origin.test/video", nil)
	initial = initial.WithContext(withRedirectCredentialTokens(initial.Context(), "gateway-token", "backend-token"))
	initial.Header.Set("X-Emby-Token", "backend-token")
	next := httptest.NewRequest(http.MethodGet, "https://cdn.test/video?token=gateway-token&token=backend-token&token=signature&api_key=x", nil)
	if err := newProxyClient(nil).CheckRedirect(next, []*http.Request{initial}); err != nil {
		t.Fatal(err)
	}
	query := next.URL.Query()
	if values := query["token"]; len(values) != 1 || values[0] != "signature" {
		t.Fatalf("token values = %v", values)
	}
	if query.Get("api_key") != "" {
		t.Fatal("api_key was not stripped")
	}
}

func TestNewServerCopiesProxyClientAndPreservesAuthClient(t *testing.T) {
	callback := func(*http.Request, []*http.Request) error { return nil }
	client := &http.Client{CheckRedirect: callback}
	callbackPointer := reflect.ValueOf(client.CheckRedirect).Pointer()
	server := NewServer(Config{HTTPClient: client}, NewMemoryStore())
	if server.client != client || server.proxyClient == client {
		t.Fatal("clients do not retain their intended ownership")
	}
	if client.CheckRedirect == nil || server.proxyClient.Transport != client.Transport {
		t.Fatal("caller client was modified or transport was not shared")
	}
	if reflect.ValueOf(client.CheckRedirect).Pointer() != callbackPointer {
		t.Fatal("caller redirect callback changed")
	}
}

func TestProxyClientCustomCallbackCanExceedDefaultRedirectLimit(t *testing.T) {
	called := false
	client := newProxyClient(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		called = true
		return nil
	}})
	req := httptest.NewRequest(http.MethodGet, "https://origin.test/video", nil)
	next := httptest.NewRequest(http.MethodGet, "https://origin.test/next", nil)
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = req
	}
	if err := client.CheckRedirect(next, via); err != nil || !called {
		t.Fatal("custom redirect callback did not retain its behavior")
	}
}

func TestProxyClientRedirectPreservesCallbackErrorAndDefaultLimit(t *testing.T) {
	want := errors.New("callback stopped redirect")
	server := NewServer(Config{HTTPClient: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return want }}}, NewMemoryStore())
	req := httptest.NewRequest(http.MethodGet, "https://origin.test/video", nil)
	next := httptest.NewRequest(http.MethodGet, "https://other.test/video", nil)
	if !errors.Is(server.proxyClient.CheckRedirect(next, []*http.Request{req}), want) {
		t.Fatal("callback error was not preserved")
	}
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = req
	}
	if err := newProxyClient(nil).CheckRedirect(next, via); err == nil {
		t.Fatal("default redirect limit was not enforced")
	}
}

func TestProxyClientRedirectChainDoesNotForwardCredentials(t *testing.T) {
	var targetSawCredentials bool
	var targetRange string
	var targetSignature string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetSawCredentials = r.Header.Get("Authorization") != "" || r.Header.Get("X-Emby-Token") != "" || r.Header.Get("Cookie") != ""
		targetRange = r.Header.Get("Range")
		targetSignature = r.URL.Query().Get("signature")
		if r.URL.Query().Get("token") != "" || r.URL.Query().Get("api_key") != "" {
			targetSawCredentials = true
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"?token=header-token&api_key=auth-token&signature=cdn", http.StatusFound)
	}))
	defer source.Close()

	req, err := http.NewRequest(http.MethodGet, source.URL, nil)
	if err != nil {
		t.Fatal("failed to create request")
	}
	req.Header.Set("Authorization", `Emby Token="auth-token"`)
	req.Header.Set("X-Emby-Token", "header-token")
	req.Header.Set("Cookie", "cookie")
	req.Header.Set("Range", "bytes=0-")
	resp, err := newProxyClient(nil).Do(req)
	if err != nil {
		t.Fatal("redirect chain request failed")
	}
	resp.Body.Close()
	if targetSawCredentials || targetRange != "bytes=0-" || targetSignature != "cdn" {
		t.Fatal("redirect target received credentials or missed range")
	}
}

func TestProxyClientRedirectChainStripsCredentialsAtEveryCrossOriginHop(t *testing.T) {
	var middleSawCredentials, targetSawCredentials bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetSawCredentials = requestHasCredentials(r)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	middle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		middleSawCredentials = requestHasCredentials(r)
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer middle.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, middle.URL, http.StatusFound)
	}))
	defer source.Close()

	req, err := http.NewRequest(http.MethodGet, source.URL, nil)
	if err != nil {
		t.Fatal("failed to create request")
	}
	req.Header.Set("Authorization", "credential")
	req.Header.Set("X-Emby-Token", "token")
	req.Header.Set("Cookie", "cookie")
	resp, err := newProxyClient(nil).Do(req)
	if err != nil {
		t.Fatal("redirect chain request failed")
	}
	resp.Body.Close()
	if middleSawCredentials || targetSawCredentials {
		t.Fatal("cross-origin redirect hop received credentials")
	}
}

func TestProxyClientRejectsCrossOrigin307And308RequestBodies(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			targetRequests := 0
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetRequests++
				w.WriteHeader(http.StatusNoContent)
			}))
			defer target.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+"?token=backend-token", status)
			}))
			defer source.Close()

			req, err := http.NewRequest(http.MethodPost, source.URL, bytes.NewBufferString("backend-token"))
			if err != nil {
				t.Fatal("failed to create request")
			}
			req.Header.Set("X-Emby-Token", "backend-token")
			_, err = newProxyClient(nil).Do(req)
			if !errors.Is(err, errCrossOriginRedirectBody) || targetRequests != 0 {
				t.Fatal("cross-origin body redirect was followed or not rejected")
			}
		})
	}
}

func TestProxyClientErrUseLastResponseDoesNotContactTarget(t *testing.T) {
	targetRequests := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()

	client := newProxyClient(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
	resp, err := client.Get(source.URL)
	if err != nil {
		t.Fatal("ErrUseLastResponse did not return redirect response")
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || targetRequests != 0 {
		t.Fatal("redirect response or target request was incorrect")
	}
}

func TestSameOriginNormalizesDefaultPorts(t *testing.T) {
	a, _ := url.Parse("HTTP://example.test/path")
	b, _ := url.Parse("http://EXAMPLE.test:80/path")
	if !sameOrigin(a, b) {
		t.Fatal("default ports were not normalized")
	}
}

func TestRedirectTokensIgnoresNonEmbyAuthorization(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://origin.test", nil)
	req.Header.Set("Authorization", "Bearer token")
	if len(redirectTokens(req)) != 0 {
		t.Fatal("non-Emby authorization was treated as a token")
	}
}

func TestSanitizeCrossOriginRedirectPreservesRawQuerySegments(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://target.test?signature=a%2Bb&name=hello+world&token=keep", nil)
	original := req.URL.RawQuery
	sanitizeCrossOriginRedirect(req, map[string]struct{}{"not-present": {}})
	if req.URL.RawQuery != original {
		t.Fatal("raw query changed without a token match")
	}

	req = httptest.NewRequest(http.MethodGet, "https://target.test?signature=a%2Bb&token=remove&name=hello+world&token=keep&broken=%ZZ", nil)
	sanitizeCrossOriginRedirect(req, map[string]struct{}{"remove": {}})
	if req.URL.RawQuery != "signature=a%2Bb&name=hello+world&token=keep&broken=%ZZ" {
		t.Fatal("raw query segments were not preserved")
	}
}

func requestHasCredentials(r *http.Request) bool {
	for name := range r.Header {
		if isCrossOriginSensitiveHeader(name) {
			return true
		}
	}
	return false
}
