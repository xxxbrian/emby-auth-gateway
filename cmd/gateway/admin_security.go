package main

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/adminauth"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
	"github.com/pocketbase/pocketbase/tools/router"
)

const (
	superuserCollectionName = "_superusers"

	// 10 attempts / minute per RealIP for PB superuser password auth.
	superuserAuthRateLimit  = 10
	superuserAuthRateWindow = time.Minute

	superuserAuthRateMiddlewareId = "gatewaySuperuserAuthRateLimit"
	// Run with other early rate-limit middleware (before default PB rate limit).
	superuserAuthRateMiddlewarePriority = apis.DefaultRateLimitMiddlewarePriority - 1
)

// validateAdminOrigin normalizes and validates GATEWAY_ADMIN_ORIGIN.
// Requires an absolute http/https URL with a host. http is allowed only for
// loopback hosts (localhost, 127.0.0.1, ::1); non-loopback http fails closed.
func validateAdminOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("GATEWAY_ADMIN_ORIGIN is empty")
	}
	// Reject path-only or scheme-less values before parse edge cases.
	if !strings.Contains(raw, "://") {
		return "", fmt.Errorf("GATEWAY_ADMIN_ORIGIN must be an absolute http or https URL")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("GATEWAY_ADMIN_ORIGIN is invalid: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("GATEWAY_ADMIN_ORIGIN must use http or https scheme")
	}
	if u.Host == "" {
		return "", fmt.Errorf("GATEWAY_ADMIN_ORIGIN must include a host")
	}
	if u.User != nil {
		return "", fmt.Errorf("GATEWAY_ADMIN_ORIGIN must not include userinfo")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("GATEWAY_ADMIN_ORIGIN must not include query or fragment")
	}
	// Origins never include a path; reject path-only leftovers and accidental paths.
	if path := strings.TrimRight(u.Path, "/"); path != "" {
		return "", fmt.Errorf("GATEWAY_ADMIN_ORIGIN must not include a path")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("GATEWAY_ADMIN_ORIGIN http is only allowed for loopback hosts")
	}

	// Canonical origin: scheme://host (no trailing slash, no path).
	return u.Scheme + "://" + u.Host, nil
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// superuserAuthCollectionIDs returns identities used in PB auth paths:
// the collection name plus the resolved collection id when available.
func superuserAuthCollectionIDs(app core.App) []string {
	ids := []string{superuserCollectionName}
	if app == nil {
		return ids
	}
	col, err := app.FindCollectionByNameOrId(superuserCollectionName)
	if err != nil || col == nil {
		return ids
	}
	if id := strings.TrimSpace(col.Id); id != "" && id != superuserCollectionName {
		ids = append(ids, id)
	}
	return ids
}

// isSuperuserAuthRateLimitedPath reports whether path is a superuser
// auth-with-password or auth-refresh endpoint for one of the given collection identities.
func isSuperuserAuthRateLimitedPath(path string, collectionIDs []string) bool {
	if len(collectionIDs) == 0 {
		collectionIDs = []string{superuserCollectionName}
	}
	for _, id := range collectionIDs {
		if id == "" {
			continue
		}
		if path == "/api/collections/"+id+"/auth-with-password" ||
			path == "/api/collections/"+id+"/auth-refresh" {
			return true
		}
	}
	return false
}

// bindSuperuserAuthRateLimit registers early middleware that rate-limits
// PocketBase superuser password auth (and refresh) by RealIP.
// collectionIDs should include "_superusers" and the resolved collection id.
func bindSuperuserAuthRateLimit(r *router.Router[*core.RequestEvent], limiter *adminauth.RateLimiter, collectionIDs ...string) {
	if r == nil {
		return
	}
	if limiter == nil {
		limiter = adminauth.NewRateLimiter(superuserAuthRateLimit, superuserAuthRateWindow)
	}
	if len(collectionIDs) == 0 {
		collectionIDs = []string{superuserCollectionName}
	}
	r.Bind(superuserAuthRateLimitMiddleware(limiter, collectionIDs))
}

func superuserAuthRateLimitMiddleware(limiter *adminauth.RateLimiter, collectionIDs []string) *hook.Handler[*core.RequestEvent] {
	// Copy so callers cannot mutate the matched set after bind.
	ids := append([]string(nil), collectionIDs...)
	return &hook.Handler[*core.RequestEvent]{
		Id:       superuserAuthRateMiddlewareId,
		Priority: superuserAuthRateMiddlewarePriority,
		Func: func(e *core.RequestEvent) error {
			if e.Request == nil || e.Request.Method != http.MethodPost {
				return e.Next()
			}
			if !isSuperuserAuthRateLimitedPath(e.Request.URL.Path, ids) {
				return e.Next()
			}
			ip := e.RealIP()
			if !limiter.Allow("ip:" + ip) {
				h := e.Response.Header()
				h.Set("Cache-Control", "no-store")
				return e.TooManyRequestsError("superuser auth rate limit exceeded", nil)
			}
			return e.Next()
		},
	}
}
