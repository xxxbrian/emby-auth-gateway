package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xxxbrian/emby-auth-gateway/internal/embyweb"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/router"
)

const testAllowedCORSOrigin = "https://app.emby.media"

func TestNewEmbyWebServerStates(t *testing.T) {
	t.Run("blank_disabled", func(t *testing.T) {
		s, err := newEmbyWebServer("", "")
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != embyweb.StateDisabled {
			t.Fatalf("state=%s", s.Status().State)
		}
	})

	t.Run("whitespace_disabled", func(t *testing.T) {
		s, err := newEmbyWebServer("  \t  ", "")
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != embyweb.StateDisabled {
			t.Fatalf("state=%s", s.Status().State)
		}
	})

	t.Run("missing_no_startup_error", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "absent")
		s, err := newEmbyWebServer(missing, "")
		if err != nil {
			t.Fatalf("missing must not fail construction: %v", err)
		}
		if s.Status().State != embyweb.StateMissing {
			t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
		}
	})

	t.Run("corrupt_file_root_no_startup_error", func(t *testing.T) {
		root := t.TempDir()
		p := filepath.Join(root, "not-a-dir")
		if err := os.WriteFile(p, []byte(`x`), 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := newEmbyWebServer(p, "")
		if err != nil {
			t.Fatalf("corrupt must not fail construction: %v", err)
		}
		if s.Status().State != embyweb.StateCorrupt {
			t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
		}
	})

	t.Run("incomplete_tree_missing", func(t *testing.T) {
		// Directory without canaries is missing (not ready), not a construction error.
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<html>"), 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := newEmbyWebServer(root, "")
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != embyweb.StateMissing {
			t.Fatalf("state=%s err=%v want missing", s.Status().State, s.Status().Err)
		}
	})

	t.Run("ready_fixture_tree", func(t *testing.T) {
		root := writeReadyWebAssets(t)
		s, err := newEmbyWebServer(root, "")
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != embyweb.StateReady {
			t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
		}
	})
}

func TestMountGatewayRoutesComposition(t *testing.T) {
	t.Run("disabled_web_404_never_api", func(t *testing.T) {
		web, err := newEmbyWebServer("", "")
		if err != nil {
			t.Fatal(err)
		}
		var apiHits atomic.Int64
		api := countingAPI(&apiHits, http.StatusUnauthorized)
		h := buildComposedHandler(t, web, api, true)

		for _, path := range []string{"/emby/web", "/emby/web/", "/emby/web/nope.js"} {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusNotFound {
				t.Fatalf("%s: code=%d body=%q", path, rr.Code, rr.Body.String())
			}
			if rr.Code == http.StatusUnauthorized {
				t.Fatalf("%s: leaked to API 401", path)
			}
		}
		if apiHits.Load() != 0 {
			t.Fatalf("api hits=%d want 0", apiHits.Load())
		}
	})

	t.Run("missing_web_503", func(t *testing.T) {
		web, err := newEmbyWebServer(filepath.Join(t.TempDir(), "missing"), "")
		if err != nil {
			t.Fatal(err)
		}
		var apiHits atomic.Int64
		h := buildComposedHandler(t, web, countingAPI(&apiHits, 401), true)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/", nil))
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("code=%d", rr.Code)
		}
		if apiHits.Load() != 0 {
			t.Fatalf("api hits=%d", apiHits.Load())
		}
	})

	t.Run("ready_redirect_index_canaries_and_api", func(t *testing.T) {
		// Use a testing-safe ready double for composition (routing/CORS/guard)
		// without needing official prepared assets or registry injection.
		web := syntheticReadyWebHandler()
		var apiHits atomic.Int64
		h := buildComposedHandler(t, web, countingAPI(&apiHits, 418), true)

		// Redirect root without slash.
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web?x=1", nil))
		if rr.Code != http.StatusPermanentRedirect {
			t.Fatalf("redirect code=%d", rr.Code)
		}
		if loc := rr.Header().Get("Location"); loc != "/emby/web/?x=1" {
			t.Fatalf("Location=%q", loc)
		}

		// Index and canaries.
		for _, path := range []string{
			"/emby/web/",
			"/emby/web/manifest.json",
			"/emby/web/index.html",
			"/emby/web/strings/en-US.json",
		} {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("%s: code=%d body=%q", path, rr.Code, rr.Body.String())
			}
		}

		// Non-Web API path still hits API.
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/Users/Public", nil))
		if rr.Code != 418 {
			t.Fatalf("api code=%d", rr.Code)
		}
		if apiHits.Load() != 1 {
			t.Fatalf("api hits=%d", apiHits.Load())
		}
	})

	t.Run("unknown_web_methods", func(t *testing.T) {
		web := syntheticReadyWebHandler()
		h := buildComposedHandler(t, web, countingAPI(new(atomic.Int64), 401), true)

		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/missing.js", nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("GET unknown: %d", rr.Code)
		}

		rr = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodOptions, "/emby/web/missing.js", nil)
		req.Header.Set("Origin", "https://evil.example")
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("OPTIONS unknown: %d (global CORS would be 204)", rr.Code)
		}
		if allow := rr.Header().Get("Allow"); allow != "GET, HEAD" {
			t.Fatalf("Allow=%q", allow)
		}
		// Must not look like global pbCors grant.
		if rr.Header().Get("Access-Control-Allow-Origin") == "*" {
			t.Fatal("unexpected global CORS *")
		}

		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/emby/web/modules/app.js", nil))
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST: %d", rr.Code)
		}
	})

	t.Run("canary_cors_unbinds_pbCors", func(t *testing.T) {
		web := syntheticReadyWebHandler()
		h := buildComposedHandler(t, web, countingAPI(new(atomic.Int64), 401), true)

		// Allowed origin simple GET.
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/emby/web/manifest.json", nil)
		req.Header.Set("Origin", testAllowedCORSOrigin)
		h.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("code=%d", rr.Code)
		}
		if rr.Header().Get("Access-Control-Allow-Origin") != testAllowedCORSOrigin {
			t.Fatalf("ACAO=%q", rr.Header().Get("Access-Control-Allow-Origin"))
		}

		// Disallowed origin: no embyweb grant (and not pbCors *).
		rr = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/emby/web/manifest.json", nil)
		req.Header.Set("Origin", "https://evil.example")
		h.ServeHTTP(rr, req)
		if acao := rr.Header().Get("Access-Control-Allow-Origin"); acao != "" {
			t.Fatalf("unexpected ACAO=%q", acao)
		}

		// Preflight with PNA for allowed origin.
		rr = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodOptions, "/emby/web/manifest.json", nil)
		req.Header.Set("Origin", testAllowedCORSOrigin)
		req.Header.Set("Access-Control-Request-Method", "GET")
		req.Header.Set("Access-Control-Request-Private-Network", "true")
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("preflight code=%d", rr.Code)
		}
		if rr.Header().Get("Access-Control-Allow-Origin") != testAllowedCORSOrigin {
			t.Fatalf("preflight ACAO=%q", rr.Header().Get("Access-Control-Allow-Origin"))
		}
		if rr.Header().Get("Access-Control-Allow-Methods") != "GET, HEAD" {
			t.Fatalf("methods=%q", rr.Header().Get("Access-Control-Allow-Methods"))
		}
		if rr.Header().Get("Access-Control-Allow-Private-Network") != "true" {
			t.Fatal("expected PNA grant")
		}

		// Control: API OPTIONS still gets global pbCors 204 with *.
		rr = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodOptions, "/emby/Users", nil)
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Access-Control-Request-Method", "GET")
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("api OPTIONS code=%d (want global CORS 204)", rr.Code)
		}
		if rr.Header().Get("Access-Control-Allow-Origin") == "" {
			t.Fatal("api OPTIONS should still have pbCors grant")
		}
	})

	t.Run("registration_order_independent", func(t *testing.T) {
		web := syntheticReadyWebHandler()
		var apiHits atomic.Int64
		api := countingAPI(&apiHits, 401)

		// Mount API first, then Web, using a custom order helper.
		app := newTestApp(t)
		pbRouter := newTestRouter(t, app, true)
		// Reverse of mountGatewayRoutes registration order.
		webExact, webWild, apiExact, apiWild := gatewayRoutePatterns()
		apiAction := func(re *core.RequestEvent) error {
			api.ServeHTTP(re.Response, re.Request)
			return nil
		}
		webAction := func(re *core.RequestEvent) error {
			web.ServeHTTP(re.Response, re.Request)
			return nil
		}
		pbRouter.Any(apiWild, apiAction)
		pbRouter.Any(apiExact, apiAction)
		pbRouter.Any(webExact, webAction).Unbind(apis.DefaultCorsMiddlewareId)
		pbRouter.Any(webWild, webAction).Unbind(apis.DefaultCorsMiddlewareId)

		mux, err := pbRouter.BuildMux()
		if err != nil {
			t.Fatal(err)
		}
		h := embyweb.GuardHandler(mux)

		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/manifest.json", nil))
		if rr.Code != 200 {
			t.Fatalf("web code=%d (specificity should win)", rr.Code)
		}
		if apiHits.Load() != 0 {
			t.Fatalf("api hits=%d", apiHits.Load())
		}
	})

	t.Run("guard_blocks_traversal_websocket_api", func(t *testing.T) {
		web := syntheticReadyWebHandler()
		var apiHits, webHits atomic.Int64
		api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiHits.Add(1)
			w.WriteHeader(418)
		})
		// Count web hits via wrapper.
		countingWeb := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			webHits.Add(1)
			web.ServeHTTP(w, r)
		})
		h := buildComposedHandler(t, countingWeb, api, true)

		// Literal traversal under web prefix: guard 404 no-store, no downstream.
		for _, path := range []string{
			"/emby/web/../Users",
			"/emby/web/./index.html",
			"/emby/web//index.html",
		} {
			apiHits.Store(0)
			webHits.Store(0)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusNotFound {
				t.Fatalf("%s: code=%d", path, rr.Code)
			}
			if rr.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%s: Cache-Control=%q", path, rr.Header().Get("Cache-Control"))
			}
			if apiHits.Load() != 0 || webHits.Load() != 0 {
				t.Fatalf("%s: api=%d web=%d want 0", path, apiHits.Load(), webHits.Load())
			}
		}

		// Encoded traversal.
		apiHits.Store(0)
		webHits.Store(0)
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
		req.URL.Path = "/emby/web/modules/../index.html"
		req.URL.RawPath = "/emby/web/modules/%2e%2e/index.html"
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("encoded: code=%d", rr.Code)
		}
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("encoded: missing no-store")
		}
		if apiHits.Load() != 0 || webHits.Load() != 0 {
			t.Fatalf("encoded: api=%d web=%d", apiHits.Load(), webHits.Load())
		}

		// /emby/websocket remains API (lookalike, not Web).
		apiHits.Store(0)
		webHits.Store(0)
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/websocket", nil))
		if rr.Code != 418 {
			t.Fatalf("websocket code=%d", rr.Code)
		}
		if apiHits.Load() != 1 {
			t.Fatalf("websocket api hits=%d", apiHits.Load())
		}
		if webHits.Load() != 0 {
			t.Fatalf("websocket web hits=%d", webHits.Load())
		}
	})

	t.Run("registration_stubs_when_web_ready", func(t *testing.T) {
		// Host-root registration stubs mount only when Web is ready
		// (same condition as root redirect).
		web := syntheticReadyWebHandler()
		var apiHits atomic.Int64
		h := buildComposedHandlerWithRootRedirect(t, web, countingAPI(&apiHits, 418), true, true)
		for _, path := range []string{
			"/admin/service/registration/validateDevice",
			"/admin/service/registration/validate",
			"/admin/service/registration/getStatus",
		} {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, path, nil)
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("%s: code=%d body=%q", path, rr.Code, rr.Body.String())
			}
			if rr.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("%s: content-type %q", path, rr.Header().Get("Content-Type"))
			}
		}
		if apiHits.Load() != 0 {
			t.Fatalf("registration must not hit API, hits=%d", apiHits.Load())
		}
	})

	t.Run("registration_stubs_absent_when_web_disabled", func(t *testing.T) {
		web, err := newEmbyWebServer("", "")
		if err != nil {
			t.Fatal(err)
		}
		// rootRedirect=false mirrors production when Web is not ready.
		var apiHits atomic.Int64
		h := buildComposedHandler(t, web, countingAPI(&apiHits, 418), true)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/service/registration/validateDevice", nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("disabled web: registration code=%d want 404", rr.Code)
		}
		if apiHits.Load() != 0 {
			t.Fatalf("registration must not hit API when unmounted, hits=%d", apiHits.Load())
		}
	})

	t.Run("registration_stubs_absent_when_web_missing", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "absent")
		web, err := newEmbyWebServer(missing, "")
		if err != nil {
			t.Fatal(err)
		}
		if webReadyForRootRedirect(web) {
			t.Fatal("missing web must not be ready for root redirect")
		}
		var apiHits atomic.Int64
		h := buildComposedHandlerWithRootRedirect(t, web, countingAPI(&apiHits, 418), true, false)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/service/registration/getStatus", nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("missing web: registration code=%d want 404", rr.Code)
		}
		if apiHits.Load() != 0 {
			t.Fatalf("registration must not hit API when unmounted, hits=%d", apiHits.Load())
		}
	})

	t.Run("blank_env_api_only", func(t *testing.T) {
		web, err := newEmbyWebServer("", "")
		if err != nil {
			t.Fatal(err)
		}
		var apiHits atomic.Int64
		h := buildComposedHandler(t, web, countingAPI(&apiHits, 200), true)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/System/Info", nil))
		if rr.Code != 200 {
			t.Fatalf("code=%d", rr.Code)
		}
		if apiHits.Load() != 1 {
			t.Fatalf("api hits=%d", apiHits.Load())
		}
		// Fixed /emby/web is disabled => 404, never API.
		apiBefore := apiHits.Load()
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/", nil))
		if rr.Code != 404 {
			t.Fatalf("fixed disabled web code=%d", rr.Code)
		}
		if apiHits.Load() != apiBefore {
			t.Fatalf("fixed web must not hit API")
		}
	})

	t.Run("root_redirect_when_web_ready", func(t *testing.T) {
		web := syntheticReadyWebHandler()
		var apiHits atomic.Int64
		h := buildComposedHandlerWithRootRedirect(t, web, countingAPI(&apiHits, 418), true, true)

		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		if rr.Code != http.StatusPermanentRedirect {
			t.Fatalf("GET / code=%d want 308", rr.Code)
		}
		if loc := rr.Header().Get("Location"); loc != "/emby/web/" {
			t.Fatalf("Location=%q", loc)
		}

		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/?x=1", nil))
		if rr.Code != http.StatusPermanentRedirect {
			t.Fatalf("GET /?x=1 code=%d want 308", rr.Code)
		}
		if loc := rr.Header().Get("Location"); loc != "/emby/web/?x=1" {
			t.Fatalf("Location=%q", loc)
		}

		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodHead, "/", nil))
		if rr.Code != http.StatusPermanentRedirect {
			t.Fatalf("HEAD / code=%d want 308", rr.Code)
		}
		if loc := rr.Header().Get("Location"); loc != "/emby/web/" {
			t.Fatalf("HEAD Location=%q", loc)
		}

		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/", nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("POST / code=%d want 404", rr.Code)
		}
		if apiHits.Load() != 0 {
			t.Fatalf("root must not hit API, hits=%d", apiHits.Load())
		}
	})

	t.Run("root_no_redirect_when_web_disabled", func(t *testing.T) {
		web, err := newEmbyWebServer("", "")
		if err != nil {
			t.Fatal(err)
		}
		if webReadyForRootRedirect(web) {
			t.Fatal("disabled web must not enable root redirect")
		}
		var apiHits atomic.Int64
		h := buildComposedHandlerWithRootRedirect(t, web, countingAPI(&apiHits, 418), true, false)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		if rr.Code == http.StatusPermanentRedirect {
			t.Fatalf("disabled web must not redirect /, Location=%q", rr.Header().Get("Location"))
		}
		if apiHits.Load() != 0 {
			t.Fatalf("root must not hit API, hits=%d", apiHits.Load())
		}
	})

	t.Run("root_no_redirect_when_web_missing", func(t *testing.T) {
		web, err := newEmbyWebServer(filepath.Join(t.TempDir(), "missing"), "")
		if err != nil {
			t.Fatal(err)
		}
		if webReadyForRootRedirect(web) {
			t.Fatal("missing web must not enable root redirect")
		}
		h := buildComposedHandlerWithRootRedirect(t, web, countingAPI(new(atomic.Int64), 418), true, false)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		if rr.Code == http.StatusPermanentRedirect {
			t.Fatalf("missing web must not redirect /, Location=%q", rr.Header().Get("Location"))
		}
	})

	t.Run("guard_encoded_web_prefix_composed", func(t *testing.T) {
		web := syntheticReadyWebHandler()
		var apiHits, webHits atomic.Int64
		api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiHits.Add(1)
			w.WriteHeader(418)
		})
		countingWeb := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			webHits.Add(1)
			web.ServeHTTP(w, r)
		})
		h := buildComposedHandler(t, countingWeb, api, true)

		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
		req.URL.Path = "/emby/web/../Users"
		req.URL.RawPath = "/emby/%77eb/../Users"
		h.ServeHTTP(rr, req)
		if rr.Code != 404 {
			t.Fatalf("code=%d", rr.Code)
		}
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("Cache-Control=%q", rr.Header().Get("Cache-Control"))
		}
		if apiHits.Load() != 0 || webHits.Load() != 0 {
			t.Fatalf("api=%d web=%d want 0", apiHits.Load(), webHits.Load())
		}
	})
}

func TestWrapServerHandler(t *testing.T) {
	t.Run("nil_server", func(t *testing.T) {
		wrapServerHandler(nil) // must not panic
	})

	t.Run("nil_handler", func(t *testing.T) {
		s := &http.Server{}
		wrapServerHandler(s)
		if s.Handler == nil {
			t.Fatal("expected defensive handler")
		}
		rr := httptest.NewRecorder()
		s.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/../x", nil))
		if rr.Code != 404 {
			t.Fatalf("code=%d", rr.Code)
		}
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("Cache-Control=%q", rr.Header().Get("Cache-Control"))
		}
	})

	t.Run("wraps_existing", func(t *testing.T) {
		var hits atomic.Int64
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(200)
		})
		s := &http.Server{Handler: inner}
		wrapServerHandler(s)
		rr := httptest.NewRecorder()
		s.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/Users", nil))
		if hits.Load() != 1 {
			t.Fatalf("hits=%d", hits.Load())
		}
		// Traversal blocked before inner.
		hits.Store(0)
		rr = httptest.NewRecorder()
		s.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/../Users", nil))
		if hits.Load() != 0 {
			t.Fatalf("traversal reached inner")
		}
		if rr.Code != 404 {
			t.Fatalf("code=%d", rr.Code)
		}
	})
}

func TestWebAssetsDirFromEnv(t *testing.T) {
	t.Setenv("GATEWAY_WEB_ASSETS_DIR", "  /tmp/assets  ")
	if got := webAssetsDirFromEnv(); got != "/tmp/assets" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("GATEWAY_WEB_ASSETS_DIR", "")
	if got := webAssetsDirFromEnv(); got != "" {
		t.Fatalf("got %q", got)
	}
}

// --- test helpers ---

func newTestApp(t *testing.T) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "test",
	})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	t.Cleanup(app.Cleanup)
	return app
}

func newTestRouter(t *testing.T, app core.App, withCORS bool) *router.Router[*core.RequestEvent] {
	t.Helper()
	pbRouter, err := apis.NewRouter(app)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if withCORS {
		// Match production Serve() binding of global pbCors.
		pbRouter.Bind(apis.CORS(apis.CORSConfig{
			AllowOrigins: []string{"*"},
			AllowMethods: []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodPost, http.MethodDelete},
		}))
	}
	return pbRouter
}

func buildComposedHandler(t *testing.T, web, api http.Handler, withCORS bool) http.Handler {
	t.Helper()
	return buildComposedHandlerWithRootRedirect(t, web, api, withCORS, false)
}

func buildComposedHandlerWithRootRedirect(t *testing.T, web, api http.Handler, withCORS, rootRedirect bool) http.Handler {
	t.Helper()
	app := newTestApp(t)
	pbRouter := newTestRouter(t, app, withCORS)
	mountGatewayRoutes(pbRouter, web, api, rootRedirect)
	mux, err := pbRouter.BuildMux()
	if err != nil {
		t.Fatalf("BuildMux: %v", err)
	}
	return embyweb.GuardHandler(mux)
}

func countingAPI(hits *atomic.Int64, code int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, "api:%s", r.URL.Path)
	})
}

// writeReadyWebAssets builds a minimal on-disk web root with required canaries.
func writeReadyWebAssets(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"index.html":         "<!doctype html><title>t</title>",
		"manifest.json":      `{"name":"test"}`,
		"strings/en-US.json": `{"Hello":"Hello"}`,
		"modules/app.js":     "console.log(1)",
	}
	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// syntheticReadyWebHandler is a testing-safe Ready double for composition tests.
// It mirrors the Ready surface needed by mount/CORS/guard composition assertions.
func syntheticReadyWebHandler() http.Handler {
	assets := map[string]string{
		"index.html":         "<!doctype html><title>t</title>",
		"manifest.json":      `{"name":"test"}`,
		"strings/en-US.json": `{"Hello":"Hello"}`,
		"modules/app.js":     "console.log(1)",
	}
	canaries := map[string]bool{
		"index.html":         true,
		"manifest.json":      true,
		"strings/en-US.json": true,
	}
	const (
		webExact = "/emby/web"
		webSlash = "/emby/web/"
	)
	writeAllow := func(w http.ResponseWriter) {
		w.Header().Set("Allow", "GET, HEAD")
	}
	applyCORS := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != testAllowedCORSOrigin {
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", testAllowedCORSOrigin)
		w.Header().Add("Vary", "Origin")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// Exact root redirect (GET/HEAD only).
		if path == webExact {
			switch r.Method {
			case http.MethodGet, http.MethodHead:
				target := webSlash
				if r.URL.RawQuery != "" {
					target = target + "?" + r.URL.RawQuery
				}
				w.Header().Set("Location", target)
				w.WriteHeader(http.StatusPermanentRedirect)
				return
			default:
				writeAllow(w)
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
		}
		if path != webSlash && !strings.HasPrefix(path, webSlash) {
			http.NotFound(w, r)
			return
		}
		rel := strings.TrimPrefix(path, webSlash)
		if path == webSlash {
			rel = "index.html"
		}
		canary := canaries[rel]
		switch r.Method {
		case http.MethodOptions:
			if !canary {
				writeAllow(w)
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			// Canary preflight surface used by composition CORS tests.
			w.Header().Set("Vary", "Origin")
			w.Header().Add("Vary", "Access-Control-Request-Method")
			w.Header().Add("Vary", "Access-Control-Request-Headers")
			w.Header().Add("Vary", "Access-Control-Request-Private-Network")
			if r.Header.Get("Origin") != testAllowedCORSOrigin {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", testAllowedCORSOrigin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD")
			if strings.EqualFold(r.Header.Get("Access-Control-Request-Private-Network"), "true") {
				w.Header().Set("Access-Control-Allow-Private-Network", "true")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodGet, http.MethodHead:
			// continue
		default:
			writeAllow(w)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, ok := assets[rel]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if canary {
			applyCORS(w, r)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write([]byte(body))
	})
}
