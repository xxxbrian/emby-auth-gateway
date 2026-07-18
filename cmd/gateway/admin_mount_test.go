package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/adminauth"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"

	"github.com/pocketbase/pocketbase/apis"
)

func TestAdminConfigFromEnv(t *testing.T) {
	t.Setenv("GATEWAY_ADMIN_ENABLED", "")
	t.Setenv("GATEWAY_ADMIN_ORIGIN", "")
	t.Setenv("GATEWAY_ADMIN_AUDIT_RETENTION_DAYS", "")
	cfg := adminConfigFromEnv()
	if cfg.Enabled {
		t.Fatal("default disabled")
	}
	if cfg.AuditRetentionDays != 30 {
		t.Fatalf("days=%d", cfg.AuditRetentionDays)
	}

	t.Setenv("GATEWAY_ADMIN_ENABLED", "1")
	t.Setenv("GATEWAY_ADMIN_ORIGIN", "https://emby.example.com/")
	t.Setenv("GATEWAY_ADMIN_AUDIT_RETENTION_DAYS", "14")
	cfg = adminConfigFromEnv()
	if !cfg.Enabled || cfg.Origin != "https://emby.example.com" || cfg.AuditRetentionDays != 14 {
		t.Fatalf("%+v", cfg)
	}
}

func TestMountAdminRequiresOrigin(t *testing.T) {
	app := newTestApp(t)
	r, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	err = mountAdmin(r, app, adminConfig{Enabled: true, Origin: ""}, nil, nil, nil, false, time.Now(), "boot")
	if err == nil || !strings.Contains(err.Error(), "GATEWAY_ADMIN_ORIGIN") {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateAdminOrigin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "https ok", in: "https://emby.example.com", want: "https://emby.example.com"},
		{name: "https trailing slash", in: "https://emby.example.com/", want: "https://emby.example.com"},
		{name: "https with port", in: "https://emby.example.com:8443", want: "https://emby.example.com:8443"},
		{name: "http loopback ipv4", in: "http://127.0.0.1:8090", want: "http://127.0.0.1:8090"},
		{name: "http localhost", in: "http://localhost", want: "http://localhost"},
		{name: "http loopback ipv6", in: "http://[::1]:8090", want: "http://[::1]:8090"},
		{name: "empty", in: "", wantErr: "empty"},
		{name: "path only", in: "/admin", wantErr: "absolute"},
		{name: "scheme-less host", in: "emby.example.com", wantErr: "absolute"},
		{name: "http non-loopback", in: "http://emby.example.com", wantErr: "loopback"},
		{name: "ftp scheme", in: "ftp://emby.example.com", wantErr: "http or https"},
		{name: "missing host", in: "https://", wantErr: "host"},
		{name: "with path", in: "https://emby.example.com/admin", wantErr: "path"},
		{name: "with query", in: "https://emby.example.com?x=1", wantErr: "query"},
		{name: "with userinfo", in: "https://user:pass@emby.example.com", wantErr: "userinfo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := validateAdminOrigin(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestMountAdminRejectsInvalidOrigin(t *testing.T) {
	app := newTestApp(t)
	r, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	err = mountAdmin(r, app, adminConfig{Enabled: true, Origin: "http://emby.example.com"}, nil, nil, nil, false, time.Now(), "boot")
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("err=%v", err)
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

func TestRegistrationPathNotCapturedBySPAWhenAdminEnabledWebDisabled(t *testing.T) {
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
	err := mountAdmin(r, app, adminConfig{
		Enabled: true,
		Origin:  "https://admin.example.test",
	}, reg, nil, nil, false /* web not ready */, time.Now().UTC(), "boot")
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

	err := mountAdmin(r, app, adminConfig{
		Enabled: true,
		Origin:  "https://admin.example.test",
	}, nil, nil, nil, true, time.Now().UTC(), "boot")
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
