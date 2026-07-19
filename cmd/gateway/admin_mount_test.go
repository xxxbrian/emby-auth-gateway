package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/adminapi"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminauth"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

func TestAdminConfigFromEnv(t *testing.T) {
	t.Setenv("GATEWAY_ADMIN_AUDIT_RETENTION_DAYS", "")
	cfg := adminConfigFromEnv()
	if cfg.AuditRetentionDays != 30 {
		t.Fatalf("days=%d", cfg.AuditRetentionDays)
	}

	t.Setenv("GATEWAY_ADMIN_AUDIT_RETENTION_DAYS", "14")
	cfg = adminConfigFromEnv()
	if cfg.AuditRetentionDays != 14 {
		t.Fatalf("%+v", cfg)
	}
}

func TestMountAdminEmptyOriginOK(t *testing.T) {
	app := newTestApp(t)
	r, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	// No GATEWAY_ADMIN_ORIGIN / PUBLIC_URL required; empty config mounts.
	if err := mountAdmin(r, app, adminConfig{}, nil, nil, nil, nil, false, time.Now(), "boot"); err != nil {
		t.Fatalf("mount with empty origin must succeed: %v", err)
	}
}

func TestMountAdminMediaBufferSnapshotWiring(t *testing.T) {
	original := newAdminAPIForMount
	t.Cleanup(func() { newAdminAPIForMount = original })
	var captured adminapi.Config
	newAdminAPIForMount = func(cfg adminapi.Config) (*adminapi.Server, error) {
		captured = cfg
		return adminapi.New(cfg)
	}
	app := newTestApp(t)
	status := telemetry.MediaBufferStatus{Enabled: true, HardBudgetBytes: 64 << 20, ActiveRequests: 2}
	callback := func() telemetry.MediaBufferStatus { return status }
	mediaBufferEnabled := func() bool { return true }
	r, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := mountAdmin(r, app, adminConfig{MediaBufferEnabled: mediaBufferEnabled}, nil, callback, nil, nil, false, time.Now(), "boot"); err != nil {
		t.Fatal(err)
	}
	if captured.MediaBufferSnapshot == nil {
		t.Fatal("media buffer callback was not forwarded")
	}
	if got := captured.MediaBufferSnapshot(); got != status {
		t.Fatalf("captured callback status=%+v want %+v", got, status)
	}
	if captured.MediaBufferEnabled == nil || !captured.MediaBufferEnabled() {
		t.Fatal("media buffer enabled callback was not forwarded")
	}

	r, err = apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := mountAdmin(r, app, adminConfig{}, nil, nil, nil, nil, false, time.Now(), "boot"); err != nil {
		t.Fatal(err)
	}
	if captured.MediaBufferSnapshot != nil {
		t.Fatal("nil media buffer callback was not preserved")
	}
	if captured.MediaBufferEnabled != nil {
		t.Fatal("nil media buffer enabled callback was not preserved")
	}
}

func TestMountedMediaBufferDisabledVersusMissingProvider(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		status  int
	}{
		{name: "disabled", enabled: false, status: http.StatusOK},
		{name: "expected provider missing", enabled: true, status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestApp(t)
			collection, err := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
			if err != nil {
				t.Fatal(err)
			}
			superuser := core.NewRecord(collection)
			superuser.SetEmail("mounted-buffer@example.test")
			superuser.SetPassword("SuperSecret1!")
			if err := app.Save(superuser); err != nil {
				t.Fatal(err)
			}
			token, err := superuser.NewAuthToken()
			if err != nil {
				t.Fatal(err)
			}
			registry := telemetry.New(nil)
			r, err := apis.NewRouter(app)
			if err != nil {
				t.Fatal(err)
			}
			if err := mountAdmin(r, app, adminConfig{MediaBufferEnabled: func() bool { return tc.enabled }}, registry, nil, nil, nil, false, time.Now(), registry.BootID()); err != nil {
				t.Fatal(err)
			}
			h, err := r.BuildMux()
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]string{"token": token})
			sessionResponse := httptest.NewRecorder()
			sessionRequest := httptest.NewRequest(http.MethodPost, "/admin/api/v1/session", bytes.NewReader(body))
			sessionRequest.Host = "admin.example.test"
			sessionRequest.Header.Set("Origin", "http://admin.example.test")
			sessionRequest.Header.Set("Content-Type", "application/json")
			h.ServeHTTP(sessionResponse, sessionRequest)
			if sessionResponse.Code != http.StatusOK {
				t.Fatalf("session: %d %s", sessionResponse.Code, sessionResponse.Body.String())
			}
			var cookie *http.Cookie
			for _, candidate := range sessionResponse.Result().Cookies() {
				if candidate.Name == adminauth.CookieDev || candidate.Name == adminauth.CookieSecure {
					cookie = candidate
					break
				}
			}
			if cookie == nil {
				t.Fatal("missing session cookie")
			}
			for _, path := range []string{"/admin/api/v1/media-buffer/streams", "/admin/api/v1/media-buffer/streams/1", "/admin/api/v1/media-buffer/series", "/admin/api/v1/media-buffer/recent"} {
				response := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodGet, path, nil)
				request.AddCookie(cookie)
				h.ServeHTTP(response, request)
				if response.Code != tc.status {
					t.Fatalf("%s: status=%d body=%s", path, response.Code, response.Body.String())
				}
				if tc.enabled && !strings.Contains(response.Body.String(), `"error":"provider_unavailable"`) {
					t.Fatalf("%s: missing bounded provider error: %s", path, response.Body.String())
				}
				if !tc.enabled && path == "/admin/api/v1/media-buffer/streams/1" && !strings.Contains(response.Body.String(), `"boot_id":"`+registry.BootID()+`","item":null`) {
					t.Fatalf("disabled detail wrapper=%s", response.Body.String())
				}
			}
		})
	}
}

func TestSuperuserAuthRateLimitMiddleware(t *testing.T) {
	app := newTestApp(t)
	r, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	// Tight limit for a fast unit test; include resolved collection id.
	ids := superuserAuthCollectionIDs(app)
	bindSuperuserAuthRateLimit(r, adminauth.NewRateLimiter(2, time.Minute), ids...)

	mux, err := r.BuildMux()
	if err != nil {
		t.Fatal(err)
	}

	passwordPath := "/api/collections/" + superuserCollectionName + "/auth-with-password"
	refreshPath := "/api/collections/" + superuserCollectionName + "/auth-refresh"

	post := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"identity":"a","password":"b"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.10:12345"
		mux.ServeHTTP(rr, req)
		return rr
	}

	// First two attempts on password auth are not rate-limited (may be 400 from PB).
	for i := 0; i < 2; i++ {
		rr := post(passwordPath)
		if rr.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d unexpectedly rate limited: body=%q", i+1, rr.Body.String())
		}
	}
	// Third attempt is blocked.
	rr := post(passwordPath)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d body=%q", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control=%q", rr.Header().Get("Cache-Control"))
	}
	if !strings.Contains(rr.Body.String(), "rate limit") {
		t.Fatalf("body=%q", rr.Body.String())
	}

	// Unrelated path is not limited by this middleware.
	rr = post("/api/health")
	if rr.Code == http.StatusTooManyRequests {
		t.Fatalf("unrelated path rate limited: body=%q", rr.Body.String())
	}

	// auth-refresh shares the same limiter key (same IP already exhausted).
	rr = post(refreshPath)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("auth-refresh expected 429, got %d body=%q", rr.Code, rr.Body.String())
	}

	// Collection-id form of the path is also rate-limited (same IP key).
	var idPath string
	for _, id := range ids {
		if id != superuserCollectionName {
			idPath = "/api/collections/" + id + "/auth-with-password"
			break
		}
	}
	if idPath == "" {
		t.Fatal("expected resolved superusers collection id")
	}
	rr = post(idPath)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("collection-id auth path expected 429, got %d body=%q", rr.Code, rr.Body.String())
	}
}

func TestIsSuperuserAuthRateLimitedPath(t *testing.T) {
	t.Parallel()
	// Fake collection id exercises the id-form matcher without needing a live app.
	ids := []string{superuserCollectionName, "pbc_fake_superusers_id"}
	passwordByName := "/api/collections/" + superuserCollectionName + "/auth-with-password"
	refreshByName := "/api/collections/" + superuserCollectionName + "/auth-refresh"
	passwordByID := "/api/collections/pbc_fake_superusers_id/auth-with-password"
	refreshByID := "/api/collections/pbc_fake_superusers_id/auth-refresh"

	if !isSuperuserAuthRateLimitedPath(passwordByName, ids) {
		t.Fatal("password path by name")
	}
	if !isSuperuserAuthRateLimitedPath(refreshByName, ids) {
		t.Fatal("refresh path by name")
	}
	if !isSuperuserAuthRateLimitedPath(passwordByID, ids) {
		t.Fatal("password path by collection id")
	}
	if !isSuperuserAuthRateLimitedPath(refreshByID, ids) {
		t.Fatal("refresh path by collection id")
	}
	if isSuperuserAuthRateLimitedPath("/api/collections/users/auth-with-password", ids) {
		t.Fatal("users collection must not match")
	}
	if isSuperuserAuthRateLimitedPath("/admin/api/v1/session", ids) {
		t.Fatal("admin session must not match")
	}
	// Default when no ids provided still matches name form.
	if !isSuperuserAuthRateLimitedPath(passwordByName, nil) {
		t.Fatal("nil ids should default to _superusers name")
	}
}

func TestRegistrationPathNotCapturedBySPAWhenAdminMountedWebDisabled(t *testing.T) {
	app := newTestApp(t)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatal(err)
	}
	r := newTestRouter(t, app, true)

	// Web disabled: no registration from mountGatewayRoutes.
	mountGatewayRoutes(r, http.NotFoundHandler(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(418)
	}), false)

	reg := telemetry.New(nil)
	err := mountAdmin(r, app, adminConfig{}, reg, nil, nil, nil, false /* web not ready */, time.Now().UTC(), "boot")
	if err != nil {
		t.Fatal(err)
	}

	mux, err := r.BuildMux()
	if err != nil {
		t.Fatal(err)
	}

	// Registration path must 404 (reserved), not SPA HTML.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/service/registration/validate", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("registration code=%d body=%q", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "Admin UI") {
		t.Fatalf("SPA captured registration path: %s", rr.Body.String())
	}

	// SPA root should serve placeholder (Static may 301 /admin → /admin/).
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("spa code=%d body=%q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Emby Auth Gateway") && !strings.Contains(rr.Body.String(), "Admin UI build pending") {
		t.Fatalf("spa body=%q", rr.Body.String())
	}

	// API path must not be SPA.
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/api/v1/overview", nil))
	if rr.Code == http.StatusOK && (strings.Contains(rr.Body.String(), "Admin UI build pending") || strings.Contains(rr.Body.String(), "Emby Auth Gateway")) {
		t.Fatal("API captured by SPA")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("api code=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestRegistrationHandlerWhenWebReadyStillWorksWithAdmin(t *testing.T) {
	app := newTestApp(t)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatal(err)
	}
	r := newTestRouter(t, app, true)

	// Web ready: registration mounted by mountGatewayRoutes.
	mountGatewayRoutes(r, http.NotFoundHandler(), http.NotFoundHandler(), true)

	err := mountAdmin(r, app, adminConfig{}, nil, nil, nil, nil, true, time.Now().UTC(), "boot")
	if err != nil {
		t.Fatal(err)
	}

	mux, err := r.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/service/registration/validate", nil))
	// Real registration handler returns 200 JSON stubs.
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestCleanupAuditLogs(t *testing.T) {
	app := newTestApp(t)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatal(err)
	}
	if err := cleanupAuditLogs(app, time.Now().UTC(), 30); err != nil {
		t.Fatal(err)
	}
}
