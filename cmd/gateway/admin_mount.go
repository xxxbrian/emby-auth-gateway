package main

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/adminapi"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminauth"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminquery"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminui"
	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

// adminConfig is loaded from environment for the public admin control plane.
// Admin is always mounted; Origin is required for CSRF (exact trusted origin).
type adminConfig struct {
	Origin             string
	AuditRetentionDays int
}

func adminConfigFromEnv() adminConfig {
	origin := resolveAdminOriginFromEnv()
	days := 30
	if v := strings.TrimSpace(os.Getenv("GATEWAY_ADMIN_AUDIT_RETENTION_DAYS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	return adminConfig{
		Origin:             origin,
		AuditRetentionDays: days,
	}
}

// resolveAdminOriginFromEnv prefers GATEWAY_ADMIN_ORIGIN; otherwise derives
// scheme://host from GATEWAY_PUBLIC_URL (path like /emby is stripped).
// Returns empty string when neither yields a usable candidate; mountAdmin
// validates and fails startup closed.
func resolveAdminOriginFromEnv() string {
	if raw := strings.TrimSpace(os.Getenv("GATEWAY_ADMIN_ORIGIN")); raw != "" {
		return strings.TrimRight(raw, "/")
	}
	if derived, err := originFromPublicURL(os.Getenv("GATEWAY_PUBLIC_URL")); err == nil {
		return derived
	}
	return ""
}

// originFromPublicURL derives scheme://host from a public base URL, stripping any path.
func originFromPublicURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("GATEWAY_PUBLIC_URL is empty")
	}
	if !strings.Contains(raw, "://") {
		return "", fmt.Errorf("GATEWAY_PUBLIC_URL must be an absolute http or https URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("GATEWAY_PUBLIC_URL is invalid: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("GATEWAY_PUBLIC_URL must use http or https scheme")
	}
	if u.Host == "" {
		return "", fmt.Errorf("GATEWAY_PUBLIC_URL must include a host")
	}
	return u.Scheme + "://" + u.Host, nil
}

// mountAdmin always mounts the admin API and SPA.
// Always reserves /admin/service/registration/{path} so the SPA cannot capture it:
// registration handler when webReady, deliberate 404 otherwise.
// acquireReconfigure is the preferred reconfigure exclusion gate; activeMediaLoad
// is the fallback when acquireReconfigure is nil.
// Origin must be a valid trusted origin (CSRF); invalid config fails startup closed.
func mountAdmin(
	r *router.Router[*core.RequestEvent],
	app core.App,
	cfg adminConfig,
	registry *telemetry.Registry,
	activeMediaLoad func() bool,
	acquireReconfigure func(force bool) (release func(), err error),
	webReady bool,
	startedAt time.Time,
	bootID string,
) error {
	if r == nil {
		return nil
	}

	// Always reserve registration path (handler or deliberate 404).
	// When web is ready, mountGatewayRoutes already mounts the real handler;
	// when not, mount an explicit 404 so SPA fallback cannot capture it.
	if !webReady {
		r.Any(fixedRegistrationWild, func(e *core.RequestEvent) error {
			http.NotFound(e.Response, e.Request)
			return nil
		})
	}

	origin, err := validateAdminOrigin(cfg.Origin)
	if err != nil {
		return fmt.Errorf("admin control plane requires a trusted origin (set GATEWAY_ADMIN_ORIGIN or a valid GATEWAY_PUBLIC_URL): %w", err)
	}
	cfg.Origin = origin

	// SPA posts to PB superuser auth directly; rate-limit before session exchange.
	// Match both collection name and resolved id (clients may use either).
	bindSuperuserAuthRateLimit(r, adminauth.NewRateLimiter(superuserAuthRateLimit, superuserAuthRateWindow), superuserAuthCollectionIDs(app)...)

	api, err := adminapi.New(adminapi.Config{
		App:                app,
		Origin:             cfg.Origin,
		Sessions:           adminauth.NewStore(adminauth.DefaultMaxSessions),
		Query:              adminquery.New(app, adminquery.DefaultConcurrency),
		Telemetry:          registry,
		AcquireReconfigure: acquireReconfigure,
		ActiveMediaLoad:    activeMediaLoad,
		StartedAt:          startedAt,
		BootID:             bootID,
	})
	if err != nil {
		return err
	}
	api.Mount(r)

	// SPA under /admin (exact + wildcard). Use Any (not GET-only) so the pattern
	// does not conflict with methodless registration routes under
	// /admin/service/registration/{path}. More-specific /admin/api/v1/* and
	// registration routes still win by path specificity.
	dist, err := fs.Sub(adminui.Dist, "dist")
	if err != nil {
		return fmt.Errorf("adminui dist: %w", err)
	}
	spa := apis.Static(dist, true)
	// Security headers for SPA HTML/assets.
	spaAction := func(e *core.RequestEvent) error {
		// Defense in depth: never serve SPA for reserved subtrees if routing ever mis-matches.
		p := e.Request.URL.Path
		if strings.HasPrefix(p, "/admin/api/") || strings.HasPrefix(p, "/admin/service/") {
			http.NotFound(e.Response, e.Request)
			return nil
		}
		h := e.Response.Header()
		h.Set("Cache-Control", "private, no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		// Never CORS * on admin SPA.
		h.Del("Access-Control-Allow-Origin")
		return spa(e)
	}
	r.Any("/admin", spaAction).Unbind(apis.DefaultCorsMiddlewareId)
	r.Any("/admin/{path...}", spaAction).Unbind(apis.DefaultCorsMiddlewareId)

	return nil
}

func cleanupAuditLogs(app core.App, now time.Time, retentionDays int) error {
	if retentionDays <= 0 {
		retentionDays = 30
	}
	cutoff := now.UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	_, err := app.DB().NewQuery("delete from audit_logs where created < {:cutoff}").Bind(map[string]any{"cutoff": cutoff}).Execute()
	return err
}
