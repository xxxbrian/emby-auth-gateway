package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	err = mountAdmin(r, app, adminConfig{Enabled: true, Origin: ""}, nil, false, time.Now(), "boot")
	if err == nil || !strings.Contains(err.Error(), "GATEWAY_ADMIN_ORIGIN") {
		t.Fatalf("err=%v", err)
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
	}, reg, false /* web not ready */, time.Now().UTC(), "boot")
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
	}, nil, true, time.Now().UTC(), "boot")
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
