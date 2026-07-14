package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
		{"media", http.MethodGet, "/Videos/i/stream", false, false},
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
		if _, ok := resourceCookieToken(req, "/Items/i/Images/Primary"); !ok {
			t.Fatalf("empty %s credential blocked cookie fallback", location)
		}
	}
	malformed := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/i/Images/Primary", nil)
	malformed.AddCookie(&http.Cookie{Name: resourceCookieName, Value: "gateway-token"})
	malformed.Header.Set("Authorization", "Bearer invalid")
	if _, ok := resourceCookieToken(malformed, "/Items/i/Images/Primary"); ok {
		t.Fatal("malformed explicit credential allowed cookie fallback")
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "http://gateway.test/emby"+tt.path, nil)
			req.AddCookie(&http.Cookie{Name: resourceCookieName, Value: "gateway-token"})
			if tt.explicit {
				req.Header.Set("X-Emby-Token", "wrong")
			}
			got, ok := resourceCookieToken(req, tt.path)
			if ok != tt.want || (ok && got != "gateway-token") {
				t.Fatalf("resourceCookieToken() = %q, %v; want cookie, %v", got, ok, tt.want)
			}
		})
	}

	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/i/Images/Primary", nil)
	req.Header.Add("Cookie", resourceCookieName+"=one; "+resourceCookieName+"=two")
	if _, ok := resourceCookieToken(req, "/Items/i/Images/Primary"); ok {
		t.Fatal("duplicate reserved cookie was accepted")
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
