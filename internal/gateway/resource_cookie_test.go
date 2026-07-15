package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type countingRoundTripper struct{ hits int }

func (t *countingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	t.hits++
	return nil, io.EOF
}

func TestResourceCookieRouteAndCredentialPolicy(t *testing.T) {
	cases := []struct {
		name     string
		method   string
		path     string
		explicit bool
		want     bool
	}{
		{"item image", http.MethodGet, "/Items/870258/Images/Primary", false, true},
		{"item image index", http.MethodHead, "/Items/870258/Images/Primary/0", false, true},
		{"user image", http.MethodGet, "/Users/u1/Images/Primary", false, true},
		{"case insensitive", http.MethodGet, "/items/i/images/primary", false, true},
		{"image metadata", http.MethodGet, "/Items/i/Images", false, false},
		{"bare media", http.MethodGet, "/Videos/i", false, false},
		{"non-decimal index", http.MethodGet, "/Items/i/Images/Primary/x", false, false},
		{"post", http.MethodPost, "/Items/i/Images/Primary", false, false},
		{"explicit invalid", http.MethodGet, "/Items/i/Images/Primary", true, false},
	}
	for _, location := range []string{"header", "query"} {
		req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/i/Images/Primary", nil)
		req.AddCookie(&http.Cookie{Name: resourceCookieName, Value: "gateway-token"})
		if location == "header" {
			req.Header.Set("X-Emby-Token", "")
		} else {
			req.URL.RawQuery = "api_key="
		}
		if _, _, ok := resourceCookieToken(req, "/Items/i/Images/Primary"); !ok {
			t.Fatalf("empty %s credential blocked cookie fallback", location)
		}
	}
	malformed := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/i/Images/Primary", nil)
	malformed.AddCookie(&http.Cookie{Name: resourceCookieName, Value: "gateway-token"})
	malformed.Header.Set("Authorization", "Bearer invalid")
	if _, _, ok := resourceCookieToken(malformed, "/Items/i/Images/Primary"); ok {
		t.Fatal("malformed explicit credential allowed cookie fallback")
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "http://gateway.test/emby"+tt.path, nil)
			req.AddCookie(&http.Cookie{Name: resourceCookieName, Value: "gateway-token"})
			if tt.explicit {
				req.Header.Set("X-Emby-Token", "wrong")
			}
			got, _, ok := resourceCookieToken(req, tt.path)
			if ok != tt.want || (ok && got != "gateway-token") {
				t.Fatalf("resourceCookieToken() = %q, %v; want cookie, %v", got, ok, tt.want)
			}
		})
	}

	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/i/Images/Primary", nil)
	req.Header.Add("Cookie", resourceCookieName+"=one; "+resourceCookieName+"=two")
	if _, _, ok := resourceCookieToken(req, "/Items/i/Images/Primary"); ok {
		t.Fatal("duplicate reserved cookie was accepted")
	}
}

func TestResourceRouteClassifierCanonicalMediaScope(t *testing.T) {
	cases := []struct {
		name, method, target string
		want                 resourceRouteKind
	}{
		{"item image", http.MethodGet, "/Items/i/Images/Primary", resourceRouteImage},
		{"user image index", http.MethodHead, "/users/u/images/primary/0", resourceRouteImage},
		{"video lower", http.MethodGet, "/videos/470657/original.mkv", resourceRouteMedia},
		{"audio descendant", http.MethodGet, "/Audio/a/subtitles/1", resourceRouteMedia},
		{"download", http.MethodGet, "/Items/i/Download", resourceRouteMedia},
		{"bare video", http.MethodGet, "/Videos/i", resourceRouteNone},
		{"ordinary item", http.MethodGet, "/Items/i", resourceRouteNone},
		{"write", http.MethodPost, "/Videos/i/file", resourceRouteNone},
		{"options", http.MethodOptions, "/Videos/i/file", resourceRouteNone},
		{"dot", http.MethodGet, "/Videos/i/../file", resourceRouteNone},
		{"image dot widening", http.MethodGet, "/Items/foo/Images/..", resourceRouteNone},
		{"repeat", http.MethodGet, "/Videos/i//file", resourceRouteNone},
		{"trailing", http.MethodGet, "/Videos/i/file/", resourceRouteNone},
		{"encoded slash", http.MethodGet, "/Videos/i%2Ffile/stream", resourceRouteNone},
		{"encoded dot", http.MethodGet, "/Videos/i/%2e/file", resourceRouteNone},
		{"encoded backslash", http.MethodGet, "/Videos/i/%5c/file", resourceRouteNone},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "http://gateway.test/emby"+tt.target, nil)
			rel, ok := (&Server{cfg: Config{GatewayBasePath: "/emby"}}).relativePath(req.URL.Path)
			if !ok {
				t.Fatal("relative path unavailable")
			}
			if got := resourceRoute(req, rel); got != tt.want {
				t.Fatalf("resourceRoute(%q) = %d, want %d", tt.target, got, tt.want)
			}
		})
	}
	upgrade := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/i/file", nil)
	upgrade.Header.Set("Connection", "Upgrade")
	upgrade.Header.Set("Upgrade", "websocket")
	if got := resourceRoute(upgrade, "/Videos/i/file"); got != resourceRouteNone {
		t.Fatalf("websocket route = %d", got)
	}
}

func TestServeHTTPRejectsUnsafeCookieResourceRoutesWithoutUpstream(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(*http.Request)
	}{
		{
			name: "websocket upgrade",
			setup: func(req *http.Request) {
				req.URL.Path = "/emby/Videos/item/file"
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", "websocket")
			},
		},
		{
			name: "image traversal widening",
			setup: func(req *http.Request) {
				// Set Path directly so URL construction cannot clean the dot segment.
				req.URL.Path = "/emby/Items/foo/Images/.."
				req.URL.RawPath = "/emby/Items/foo/Images/%2E%2E"
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			transport := &countingRoundTripper{}
			store := NewMemoryStore()
			// An active session ensures a classifier regression reaches proxying
			// instead of being masked by activeSession's unauthorized response.
			store.Sessions[HashToken("gateway-token")] = testSession("http://backend.test/emby")
			server := NewServer(Config{GatewayBasePath: "/emby", HTTPClient: &http.Client{Transport: transport}}, store)
			req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/placeholder", nil)
			tt.setup(req)
			req.AddCookie(&http.Cookie{Name: resourceCookieName, Value: "gateway-token"})
			writer := httptest.NewRecorder()
			server.ServeHTTP(writer, req)
			if writer.Code != http.StatusUnauthorized || transport.hits != 0 {
				t.Fatalf("status/upstream hits = %d/%d", writer.Code, transport.hits)
			}
		})
	}
}

func TestResourceCookieStripping(t *testing.T) {
	h := http.Header{"Cookie": []string{"other=keep; " + resourceCookieName + "=remove; another=yes"}}
	stripResourceCookie(h)
	if got := h.Get("Cookie"); got != "other=keep; another=yes" {
		t.Fatalf("Cookie = %q", got)
	}
}

func TestResourceCookieAuthenticatesImageWithoutForwardingCookie(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Items/item/Images/Primary" || r.Header.Get("X-Emby-Token") != "backend-token" {
			t.Fatalf("backend request = %s token=%q", r.URL.Path, r.Header.Get("X-Emby-Token"))
		}
		if r.Header.Get("Cookie") != "other=keep" {
			t.Fatalf("backend Cookie = %q", r.Header.Get("Cookie"))
		}
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write([]byte("image"))
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()
	req, err := http.NewRequest(http.MethodGet, gw.URL+"/emby/Items/item/Images/Primary", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", "other=keep; "+resourceCookieName+"=gateway-token")
	resp, err := testHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "private, no-store" || resp.Header.Get("Vary") != "Cookie" {
		t.Fatalf("status/cache/vary = %d/%q/%q", resp.StatusCode, resp.Header.Get("Cache-Control"), resp.Header.Get("Vary"))
	}
}

func TestResourceCookieAuthenticatesNativeMediaRange(t *testing.T) {
	var hits int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/emby/videos/470657/original.mkv" || r.Header.Get("X-Emby-Token") != "backend-token" {
			t.Fatalf("backend path/token = %s/%q", r.URL.Path, r.Header.Get("X-Emby-Token"))
		}
		if r.Header.Get("Range") != "bytes=0-" || r.Header.Get("If-Range") != `"tag"` || r.Header.Get("Cookie") != "" {
			t.Fatalf("backend range/if-range/cookie = %q/%q/%q", r.Header.Get("Range"), r.Header.Get("If-Range"), r.Header.Get("Cookie"))
		}
		q := r.URL.Query()
		if q.Get("MediaSourceId") != "source" || q.Get("Static") != "true" || q.Get("exp") != "1" || q.Get("sig") != "signed" {
			t.Fatalf("backend query = %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Range", "bytes 0-3/4")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"tag"`)
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
		w.Header().Set("Content-Length", "4")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("data"))
	}))
	defer backend.Close()
	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	url := gw.URL + "/emby/videos/470657/original.mkv?MediaSourceId=source&Static=true&exp=1&sig=signed"
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Range", "bytes=0-")
	req.Header.Set("If-Range", `"tag"`)
	req.AddCookie(&http.Cookie{Name: resourceCookieName, Value: "gateway-token"})
	resp := do(t, req)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || string(body) != "data" || resp.Header.Get("Content-Length") != "4" || resp.Header.Get("Cache-Control") != "private" || resp.Header.Get("Vary") != "Cookie" || resp.Header.Get("Content-Range") != "bytes 0-3/4" || resp.Header.Get("Accept-Ranges") != "bytes" || resp.Header.Get("ETag") != `"tag"` || resp.Header.Get("Last-Modified") != "Wed, 21 Oct 2015 07:28:00 GMT" {
		t.Fatalf("media response = %d %q headers=%#v", resp.StatusCode, body, resp.Header)
	}
	head, _ := http.NewRequest(http.MethodHead, url, nil)
	head.Header.Set("Range", "bytes=0-")
	head.Header.Set("If-Range", `"tag"`)
	head.AddCookie(&http.Cookie{Name: resourceCookieName, Value: "gateway-token"})
	headResp := do(t, head)
	headBody, _ := io.ReadAll(headResp.Body)
	_ = headResp.Body.Close()
	if headResp.StatusCode != http.StatusPartialContent || len(headBody) != 0 {
		t.Fatalf("HEAD = %d body=%q", headResp.StatusCode, headBody)
	}
	invalid, _ := http.NewRequest(http.MethodGet, url, nil)
	invalid.Header.Set("X-Emby-Token", "bad")
	invalid.AddCookie(&http.Cookie{Name: resourceCookieName, Value: "gateway-token"})
	invalidResp := do(t, invalid)
	_ = invalidResp.Body.Close()
	if invalidResp.StatusCode != http.StatusUnauthorized || hits != 2 {
		t.Fatalf("invalid status/hits = %d/%d", invalidResp.StatusCode, hits)
	}
	duplicate, _ := http.NewRequest(http.MethodGet, url, nil)
	duplicate.Header.Add("Cookie", resourceCookieName+"=gateway-token")
	duplicate.Header.Add("Cookie", resourceCookieName+"=other")
	duplicateResp := do(t, duplicate)
	_ = duplicateResp.Body.Close()
	if duplicateResp.StatusCode != http.StatusUnauthorized || hits != 2 {
		t.Fatalf("duplicate status/hits = %d/%d", duplicateResp.StatusCode, hits)
	}
}

func TestResourceCookieCachePolicy(t *testing.T) {
	cases := []struct {
		kind   resourceRouteKind
		status int
		cache  string
		vary   string
		want   string
	}{
		{resourceRouteImage, http.StatusOK, "public, max-age=1", "Origin", "private, no-store"},
		{resourceRouteMedia, http.StatusPartialContent, "public, max-age=1", "Origin", "private"},
		{resourceRouteMedia, http.StatusOK, "no-store", "Origin", "private, no-store"},
		{resourceRouteMedia, http.StatusBadGateway, "public", "*", "private, no-store"},
	}
	for _, tt := range cases {
		h := http.Header{"Cache-Control": []string{tt.cache}, "Vary": []string{tt.vary}}
		applyResourceCachePolicy(h, tt.kind, tt.status)
		if h.Get("Cache-Control") != tt.want || (tt.vary == "*" && h.Get("Vary") != "*") || (tt.vary != "*" && h.Get("Vary") != "Origin, Cookie") {
			t.Fatalf("cache/vary = %q/%q", h.Get("Cache-Control"), h.Get("Vary"))
		}
	}
	for _, values := range [][]string{{"public", "no-store"}, {"public, NO-STORE, max-age=1"}, {"public, x-no-store"}} {
		h := http.Header{"Cache-Control": values}
		applyResourceCachePolicy(h, resourceRouteMedia, http.StatusOK)
		want := "private"
		if !strings.Contains(values[len(values)-1], "x-no-store") && (len(values) > 1 || strings.Contains(values[0], "NO-STORE")) {
			want = "private, no-store"
		}
		if h.Get("Cache-Control") != want {
			t.Fatalf("cache values %#v = %q, want %q", values, h.Get("Cache-Control"), want)
		}
	}
}

func TestResourceCachePolicyAppliesToEveryCredentialSource(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/forbidden") {
			w.Header().Set("Cache-Control", "public, max-age=3600")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if strings.Contains(r.URL.Path, "/Images/") {
			w.Header().Set("Content-Type", "image/gif")
			w.Header().Set("Cache-Control", "public, max-age=3600")
			_, _ = w.Write([]byte("image"))
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write([]byte("media"))
	}))
	defer backend.Close()
	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	for _, auth := range []struct {
		name  string
		apply func(*http.Request)
	}{
		{"query", func(r *http.Request) { r.URL.RawQuery = "api_key=gateway-token" }},
		{"header", func(r *http.Request) { r.Header.Set("X-Emby-Token", "gateway-token") }},
		{"cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: resourceCookieName, Value: "gateway-token"}) }},
	} {
		for _, resource := range []struct {
			path, wantCache string
		}{
			{"/Items/item/Images/Primary", "private, no-store"},
			{"/Videos/item/stream", "private"},
		} {
			t.Run(auth.name+resource.path, func(t *testing.T) {
				req := mustRequest(t, http.MethodGet, gw.URL+"/emby"+resource.path, nil)
				auth.apply(req)
				resp := do(t, req)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != resource.wantCache {
					t.Fatalf("status/cache = %d/%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
				}
			})
		}
	}
	for _, path := range []string{"/Items/item/Images/Primary", "/Videos/item/stream"} {
		req := mustRequest(t, http.MethodGet, gw.URL+"/emby"+path, nil)
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Cache-Control") != "private, no-store" {
			t.Fatalf("unauthenticated %s status/cache = %d/%q", path, resp.StatusCode, resp.Header.Get("Cache-Control"))
		}
	}
	forbidden := mustRequest(t, http.MethodGet, gw.URL+"/emby/Videos/item/forbidden?api_key=gateway-token", nil)
	forbiddenResp := do(t, forbidden)
	_ = forbiddenResp.Body.Close()
	if forbiddenResp.StatusCode != http.StatusForbidden || forbiddenResp.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("forbidden status/cache = %d/%q", forbiddenResp.StatusCode, forbiddenResp.Header.Get("Cache-Control"))
	}
}

func TestCookieOnlyManifestAndChildrenDoNotExposeTokens(t *testing.T) {
	var backendURL string
	var childHits int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Emby-Token") != "backend-token" || r.Header.Get("Cookie") != "" {
			t.Fatalf("backend auth/cookie = %q/%q", r.Header.Get("X-Emby-Token"), r.Header.Get("Cookie"))
		}
		switch r.URL.Path {
		case "/emby/Videos/item/master.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Header().Set("Location", backendURL+"/emby/Videos/item/redirect?api_key=backend-token")
			w.Header().Set("Content-Location", backendURL+"/emby/Videos/item/content?api_key=backend-token")
			_, _ = w.Write([]byte(backendURL + "/emby/Videos/item/seg.ts?api_key=backend-token\n#EXT-X-KEY:URI=\"" + backendURL + "/emby/Videos/item/key?api_key=backend-token\"\n"))
		case "/emby/Videos/item/seg.ts", "/emby/Videos/item/subtitles/1":
			childHits++
			_, _ = w.Write([]byte("child"))
		default:
			t.Fatalf("unexpected backend path %q", r.URL.Path)
		}
	}))
	defer backend.Close()
	backendURL = backend.URL
	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()
	manifestReq, _ := http.NewRequest(http.MethodGet, gw.URL+"/emby/Videos/item/master.m3u8", nil)
	manifestReq.AddCookie(&http.Cookie{Name: resourceCookieName, Value: "gateway-token"})
	manifestResp := do(t, manifestReq)
	manifest, _ := io.ReadAll(manifestResp.Body)
	_ = manifestResp.Body.Close()
	if manifestResp.StatusCode != http.StatusOK || strings.Contains(string(manifest), "backend-token") || strings.Contains(string(manifest), "gateway-token") || strings.Contains(manifestResp.Header.Get("Location"), "backend-token") || strings.Contains(manifestResp.Header.Get("Content-Location"), "backend-token") {
		t.Fatalf("manifest/header leaked token: %q %#v", manifest, manifestResp.Header)
	}
	for _, child := range []string{"seg.ts?api_key=", "subtitles/1"} {
		req, _ := http.NewRequest(http.MethodGet, gw.URL+"/emby/Videos/item/"+child, nil)
		req.AddCookie(&http.Cookie{Name: resourceCookieName, Value: "gateway-token"})
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("child %q status=%d", child, resp.StatusCode)
		}
	}
	if childHits != 2 {
		t.Fatalf("child hits = %d", childHits)
	}
}

func TestLoginIssuesAndLogoutClearsResourceCookie(t *testing.T) {
	var backendCookie string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Users/AuthenticateByName":
			writeTestJSON(w, map[string]any{"AccessToken": "backend-token", "User": map[string]any{"Id": "backend-user"}})
		case "/emby/Items/870258/Images/Primary":
			if got := r.Header.Get("X-Emby-Token"); got != "backend-token" {
				t.Fatalf("image backend token = %q", got)
			}
			backendCookie = r.Header.Get("Cookie")
			w.Header().Set("Content-Type", "image/gif")
			_, _ = w.Write([]byte("image"))
		default:
			t.Fatalf("unexpected backend request %s", r.URL.Path)
		}
	}))
	defer backend.Close()
	store := testStore(backend.URL + "/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	login, err := http.NewRequest(http.MethodPost, gw.URL+"/emby/Users/AuthenticateByName", strings.NewReader(`{"Username":"alice","Pw":"alice-pass"}`))
	if err != nil {
		t.Fatal(err)
	}
	login.Header.Set("Content-Type", "application/json")
	resp, err := testHTTPClient.Do(login)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("login status/cache = %d/%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	token := result["AccessToken"].(string)
	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].Name != resourceCookieName || cookies[0].Path != "/emby" || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Value != token || cookies[0].MaxAge <= 0 {
		t.Fatalf("resource cookie = %#v", cookies)
	}
	image, err := http.NewRequest(http.MethodGet, gw.URL+"/emby/Items/870258/Images/Primary", nil)
	if err != nil {
		t.Fatal(err)
	}
	image.AddCookie(cookies[0])
	imageResp, err := testHTTPClient.Do(image)
	if err != nil {
		t.Fatal(err)
	}
	_ = imageResp.Body.Close()
	if imageResp.StatusCode != http.StatusOK || backendCookie != "" {
		t.Fatalf("tokenless image status/backend cookie = %d/%q", imageResp.StatusCode, backendCookie)
	}

	logout, err := http.NewRequest(http.MethodPost, gw.URL+"/emby/Sessions/Logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	logout.Header.Set("X-Emby-Token", token)
	logout.AddCookie(cookies[0])
	logoutResp, err := testHTTPClient.Do(logout)
	if err != nil {
		t.Fatal(err)
	}
	defer logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusOK || len(logoutResp.Cookies()) != 1 || logoutResp.Cookies()[0].Name != resourceCookieName || logoutResp.Cookies()[0].MaxAge >= 0 {
		t.Fatalf("logout status/cookies = %d/%#v", logoutResp.StatusCode, logoutResp.Cookies())
	}
}
