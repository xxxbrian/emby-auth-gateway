package main

import (
	"net/http"
	"os"
	"strings"

	"github.com/xxxbrian/emby-auth-gateway/internal/embyweb"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

// Fixed Web v1 mount points. Official Emby Web always lives under /emby/web
// regardless of the configurable API GATEWAY_BASE_PATH.
const (
	fixedWebExact = "/emby/web"
	fixedWebWild  = "/emby/web/{path...}"
)

// mountGatewayRoutes registers Emby Web and API catch-all routes on the
// PocketBase router. Web routes are always fixed at /emby/web (methodless Any),
// outrank an /emby API catch-all by path specificity, and unbind global pbCors so
// embyweb owns canary OPTIONS/CORS. API routes remain derived from basePath.
// The request is forwarded untouched.
func mountGatewayRoutes(
	r *router.Router[*core.RequestEvent],
	basePath string,
	web http.Handler,
	api http.Handler,
) {
	if r == nil {
		return
	}
	basePath = normalizeGatewayBasePath(basePath)
	if web == nil {
		web = http.NotFoundHandler()
	}
	if api == nil {
		api = http.NotFoundHandler()
	}

	webAction := func(re *core.RequestEvent) error {
		web.ServeHTTP(re.Response, re.Request)
		return nil
	}
	apiAction := func(re *core.RequestEvent) error {
		api.ServeHTTP(re.Response, re.Request)
		return nil
	}

	apiExact, apiWild := apiRoutePatterns(basePath)

	// Fixed Web routes first for readability; ServeMux specificity (not
	// registration order) decides /emby/web vs /emby/{path...} when base is /emby.
	r.Any(fixedWebExact, webAction).Unbind(apis.DefaultCorsMiddlewareId)
	r.Any(fixedWebWild, webAction).Unbind(apis.DefaultCorsMiddlewareId)
	r.Any(apiWild, apiAction)
	r.Any(apiExact, apiAction)
}

// apiRoutePatterns returns exact and wildcard patterns for the API catch-all
// derived from the configurable gateway base path.
func apiRoutePatterns(basePath string) (apiExact, apiWild string) {
	basePath = normalizeGatewayBasePath(basePath)
	if basePath == "/" {
		return "/", "/{path...}"
	}
	return basePath, basePath + "/{path...}"
}

// gatewayRoutePatterns returns fixed Web patterns plus API patterns for tests.
func gatewayRoutePatterns(basePath string) (webExact, webWild, apiExact, apiWild string) {
	apiExact, apiWild = apiRoutePatterns(basePath)
	return fixedWebExact, fixedWebWild, apiExact, apiWild
}

// newEmbyWebServer builds the read-only Web handler from a gateway base path and
// assets root (typically GATEWAY_WEB_ASSETS_DIR). Blank/whitespace assets root
// yields a disabled handler. Missing/corrupt trees succeed construction and
// serve 503. An enabled non-/emby base returns a constructor error.
func newEmbyWebServer(basePath, assetsRoot string) (*embyweb.Server, error) {
	return embyweb.New(embyweb.Config{
		GatewayBasePath: basePath,
		AssetsRoot:      strings.TrimSpace(assetsRoot),
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
