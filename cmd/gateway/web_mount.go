package main

import (
	"net/http"
	"os"
	"strings"

	"github.com/xxxbrian/emby-auth-gateway/internal/embyweb"
	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

// Fixed mount points. Emby API and official Emby Web always live under /emby.
const (
	fixedGatewayBasePath = "/emby"

	fixedWebExact = "/emby/web"
	fixedWebSlash = "/emby/web/"
	fixedWebWild  = "/emby/web/{path...}"

	fixedAPIExact = "/emby"
	fixedAPIWild  = "/emby/{path...}"

	// Host-root mb3admin-compatible stubs (used after Emby Web host injection).
	fixedRegistrationWild = "/admin/service/registration/{path}"
)

// mountGatewayRoutes registers Emby Web, registration stubs, and API catch-all
// routes on the PocketBase router. Web routes are always fixed at /emby/web
// (methodless Any), outrank an /emby API catch-all by path specificity, and
// unbind global pbCors so embyweb owns canary OPTIONS/CORS. Registration stubs
// are fixed at host-root /admin/service/registration/* (not under /emby).
// When redirectRootToWeb is true (Web ready), GET/HEAD / 308 to /emby/web/.
// The request is forwarded untouched.
func mountGatewayRoutes(
	r *router.Router[*core.RequestEvent],
	web http.Handler,
	api http.Handler,
	redirectRootToWeb bool,
) {
	if r == nil {
		return
	}
	if web == nil {
		web = http.NotFoundHandler()
	}
	if api == nil {
		api = http.NotFoundHandler()
	}
	reg := gateway.RegistrationHandler{}

	webAction := func(re *core.RequestEvent) error {
		web.ServeHTTP(re.Response, re.Request)
		return nil
	}
	apiAction := func(re *core.RequestEvent) error {
		api.ServeHTTP(re.Response, re.Request)
		return nil
	}
	regAction := func(re *core.RequestEvent) error {
		reg.ServeHTTP(re.Response, re.Request)
		return nil
	}

	// Fixed Web routes first for readability; ServeMux specificity (not
	// registration order) decides /emby/web vs /emby/{path...}.
	r.Any(fixedWebExact, webAction).Unbind(apis.DefaultCorsMiddlewareId)
	r.Any(fixedWebWild, webAction).Unbind(apis.DefaultCorsMiddlewareId)
	r.Any(fixedRegistrationWild, regAction)
	r.Any(fixedAPIWild, apiAction)
	r.Any(fixedAPIExact, apiAction)

	if redirectRootToWeb {
		r.Any("/", rootRedirectToWebAction)
	}
}

// rootRedirectToWebAction sends GET/HEAD / to the canonical Emby Web root.
// Other methods return 404 so host-root stays inert for non-browser traffic.
func rootRedirectToWebAction(re *core.RequestEvent) error {
	req := re.Request
	switch req.Method {
	case http.MethodGet, http.MethodHead:
		target := fixedWebSlash
		if req.URL.RawQuery != "" {
			target = target + "?" + req.URL.RawQuery
		}
		re.Response.Header().Set("Location", target)
		re.Response.WriteHeader(http.StatusPermanentRedirect) // 308
		return nil
	default:
		http.NotFound(re.Response, req)
		return nil
	}
}

// webReadyForRootRedirect reports whether Web is ready for host-root redirect.
func webReadyForRootRedirect(web *embyweb.Server) bool {
	return web != nil && web.Status().State == embyweb.StateReady
}

// gatewayRoutePatterns returns fixed Web and API patterns for tests.
func gatewayRoutePatterns() (webExact, webWild, apiExact, apiWild string) {
	return fixedWebExact, fixedWebWild, fixedAPIExact, fixedAPIWild
}

// newEmbyWebServer builds the read-only Web handler from an assets root
// (typically GATEWAY_WEB_ASSETS_DIR). Blank/whitespace assets root yields a
// disabled handler. Missing/corrupt trees succeed construction and serve 503.
// publicBaseURL (typically GATEWAY_PUBLIC_URL) supplies the fallback host for
// serve-time JS host injection when the request has no Host.
func newEmbyWebServer(assetsRoot, publicBaseURL string) (*embyweb.Server, error) {
	return embyweb.New(embyweb.Config{
		GatewayBasePath: fixedGatewayBasePath,
		AssetsRoot:      strings.TrimSpace(assetsRoot),
		PublicBaseURL:   strings.TrimSpace(publicBaseURL),
	})
}

// webAssetsDirFromEnv reads GATEWAY_WEB_ASSETS_DIR (sole Web configuration env).
func webAssetsDirFromEnv() string {
	return strings.TrimSpace(os.Getenv("GATEWAY_WEB_ASSETS_DIR"))
}

// wrapServerHandler applies embyweb.GuardHandler after the ServeMux is built.
// Nil server is a no-op; nil handler is replaced with a defensive 404 guard.
func wrapServerHandler(server *http.Server) {
	if server == nil {
		return
	}
	next := server.Handler
	if next == nil {
		next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
	}
	server.Handler = embyweb.GuardHandler(next)
}
