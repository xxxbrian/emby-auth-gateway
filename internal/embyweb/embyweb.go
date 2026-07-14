// Package embyweb serves a validated, read-only Emby Web asset tree.
//
// The package loads an immutable snapshot at construction time and never
// reopens disk files while serving. Composition layers mount the handler under
// the exact /emby/web prefix; this package independently validates request paths.
package embyweb

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
)

// SchemaVersion is the only supported current.json / install.json schema.
const SchemaVersion = 1

// Config configures a Web asset server.
//
// GatewayBasePath must normalize to "/emby" when AssetsRoot is non-empty.
// Empty or whitespace-only AssetsRoot disables the server (always 404).
type Config struct {
	GatewayBasePath string
	AssetsRoot      string
}

// State is the load outcome of a Server.
type State uint8

const (
	// StateDisabled means no assets root was configured.
	StateDisabled State = iota
	// StateMissing means a required path is absent.
	StateMissing
	// StateCorrupt means assets are present but unsafe, malformed, or inconsistent.
	StateCorrupt
	// StateReady means a verified immutable snapshot is pinned in memory.
	StateReady
)

func (s State) String() string {
	switch s {
	case StateDisabled:
		return "disabled"
	case StateMissing:
		return "missing"
	case StateCorrupt:
		return "corrupt"
	case StateReady:
		return "ready"
	default:
		return fmt.Sprintf("state(%d)", uint8(s))
	}
}

// Status reports the pinned load outcome.
type Status struct {
	State         State
	Release       string
	CatalogSHA256 string
	Err           error
}

// Server is an immutable, read-only Emby Web asset handler.
type Server struct {
	status Status
	assets map[string]*asset // path relative to files/, no leading slash
}

type asset struct {
	path       string
	data       []byte
	sha256     string
	mediaType  string
	cacheClass string
	etag       string
}

// canaryRelativePaths are the app.emby.media manual-server validation paths,
// relative to the /web/ root (not including the /emby prefix).
var canaryRelativePaths = []string{
	"manifest.json",
	"index.html",
	"strings/en-US.json",
}

const (
	webPrefix       = "/emby/web"
	webPrefixSlash  = "/emby/web/"
	requiredBase    = "/emby"
	allowedCORSOrig = "https://app.emby.media"

	cacheRevalidate = "revalidate"
	cacheImmutable  = "immutable"

	maxPointerBytes  = 64 << 10 // 64 KiB
	maxManifestBytes = 4 << 20  // 4 MiB
	maxEntries       = 4096
	maxFileBytes     = 64 << 20  // 64 MiB
	maxTotalBytes    = 256 << 20 // 256 MiB

	// Tree inventory bounds sized for the known ~868-file Emby Web tree with
	// headroom, not attacker-controlled directory growth. Inventory allocates
	// only manifest-derived expected sets plus a fixed ReadDir batch.
	//
	// maxPathBytes: max UTF-8 length of a relative asset path.
	// maxPathDepth: max slash-separated segments in an asset path.
	// maxDirs: max distinct parent directories implied by entries (plus root).
	// readDirBatch: fixed batch size for directory reads (never ReadDir(-1)).
	maxPathBytes = 512
	maxPathDepth = 24
	maxDirs      = 2048
	readDirBatch = 128
)

// CanaryPaths returns a defensive copy of the relative canary paths under /web/.
func CanaryPaths() []string {
	out := make([]string, len(canaryRelativePaths))
	copy(out, canaryRelativePaths)
	return out
}

// New constructs a Server from cfg using the immutable production catalog registry.
// Configured trees whose catalog_sha256 is not in the production registry are
// StateCorrupt (untrusted) and never Ready.
//
// Disabled (empty AssetsRoot) always succeeds. Missing and corrupt asset trees
// also succeed construction and serve 503. An enabled configuration with a
// non-/emby base path returns an error.
func New(cfg Config) (*Server, error) {
	return newWithRegistry(cfg, getProductionRegistry())
}

// newWithRegistry is the package-private constructor used by tests to inject a
// synthetic trusted catalog registry. Production code must call New.
func newWithRegistry(cfg Config, reg *catalogRegistry) (*Server, error) {
	root := strings.TrimSpace(cfg.AssetsRoot)
	if root == "" {
		return &Server{
			status: Status{State: StateDisabled},
			assets: nil,
		}, nil
	}

	base := normalizeBasePath(cfg.GatewayBasePath)
	if base != requiredBase {
		return nil, fmt.Errorf("embyweb: enabled assets require GatewayBasePath %q, got %q", requiredBase, base)
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return &Server{
			status: Status{State: StateCorrupt, Err: fmt.Errorf("resolve assets root: %w", err)},
		}, nil
	}

	if reg == nil {
		reg = getProductionRegistry()
	}
	status, assets := loadAssets(abs, reg)
	return &Server{status: status, assets: assets}, nil
}

// Status returns the pinned load status.
func (s *Server) Status() Status {
	if s == nil {
		return Status{State: StateDisabled}
	}
	return s.status
}

func normalizeBasePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s == nil {
		http.NotFound(w, r)
		return
	}

	switch s.status.State {
	case StateDisabled:
		http.NotFound(w, r)
		return
	case StateMissing, StateCorrupt:
		s.serveUnavailable(w, r)
		return
	case StateReady:
		s.serveReady(w, r)
		return
	default:
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}
}
