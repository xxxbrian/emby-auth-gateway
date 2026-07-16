// Package embyweb serves a trusted-on-disk Emby Web asset tree under /emby/web.
//
// The operator supplies the static files (for example from an emby-web-static
// Release). This package does not install, catalog, or hash-verify assets; it
// only checks canary presence at construction and serves files read-only.
package embyweb

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Config configures a Web asset server.
//
// GatewayBasePath must normalize to "/emby" when AssetsRoot is non-empty.
// Empty or whitespace-only AssetsRoot disables the server (always 404).
// PublicBaseURL is optional; its host is used as a fallback when host-inject
// paths are served without a request Host (typically GATEWAY_PUBLIC_URL).
type Config struct {
	GatewayBasePath string
	AssetsRoot      string
	PublicBaseURL   string
}

// State is the load outcome of a Server.
type State uint8

const (
	// StateDisabled means no assets root was configured.
	StateDisabled State = iota
	// StateMissing means the root or required canary files are absent.
	StateMissing
	// StateCorrupt means the root exists but is not a usable directory.
	StateCorrupt
	// StateReady means canaries are present and files may be served from disk.
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

// Status reports the construction-time readiness outcome.
type Status struct {
	State State
	Root  string
	Err   error
}

// Server is a read-only Emby Web asset handler backed by a directory on disk.
type Server struct {
	status       Status
	root         string       // absolute assets root when Ready
	dir          *anchoredDir // root-anchored FD; all serve opens are openat+O_NOFOLLOW
	fallbackHost string       // host[:port] from PublicBaseURL, may be empty
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

	maxPathBytes       = 512
	maxPathDepth       = 24
	maxInjectFileBytes = 8 << 20 // 8 MiB; inject targets are small JS
)

// CanaryPaths returns a defensive copy of the relative canary paths under /web/.
func CanaryPaths() []string {
	out := make([]string, len(canaryRelativePaths))
	copy(out, canaryRelativePaths)
	return out
}

// New constructs a Server from cfg.
//
// Disabled (empty AssetsRoot) always succeeds. Missing and corrupt trees also
// succeed construction and serve 503 (or 404 when disabled). An enabled
// configuration with a non-/emby base path returns an error.
func New(cfg Config) (*Server, error) {
	fallbackHost := hostFromPublicURL(cfg.PublicBaseURL)

	root := strings.TrimSpace(cfg.AssetsRoot)
	if root == "" {
		return &Server{
			status:       Status{State: StateDisabled},
			fallbackHost: fallbackHost,
		}, nil
	}

	base := normalizeBasePath(cfg.GatewayBasePath)
	if base != requiredBase {
		return nil, fmt.Errorf("embyweb: enabled assets require GatewayBasePath %q, got %q", requiredBase, base)
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return &Server{
			status:       Status{State: StateCorrupt, Err: fmt.Errorf("resolve assets root: %w", err)},
			fallbackHost: fallbackHost,
		}, nil
	}

	st, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return &Server{
				status:       Status{State: StateMissing, Root: abs, Err: err},
				fallbackHost: fallbackHost,
			}, nil
		}
		return &Server{
			status:       Status{State: StateCorrupt, Root: abs, Err: err},
			fallbackHost: fallbackHost,
		}, nil
	}
	// Reject non-directories and symlink roots (Lstat of a symlink is never IsDir).
	if !st.IsDir() || st.Mode()&fs.ModeSymlink != 0 {
		return &Server{
			status: Status{
				State: StateCorrupt,
				Root:  abs,
				Err:   fmt.Errorf("assets root is not a directory"),
			},
			fallbackHost: fallbackHost,
		}, nil
	}

	// Anchor the root with O_DIRECTORY|O_NOFOLLOW so serve-time openat walks
	// cannot follow intermediate or final symlinks out of the tree.
	dir, err := openAnchoredDir(abs)
	if err != nil {
		return &Server{
			status: Status{
				State: StateCorrupt,
				Root:  abs,
				Err:   fmt.Errorf("anchor assets root: %w", err),
			},
			fallbackHost: fallbackHost,
		}, nil
	}

	if err := checkCanaries(dir); err != nil {
		_ = dir.Close()
		return &Server{
			status:       Status{State: StateMissing, Root: abs, Err: err},
			fallbackHost: fallbackHost,
		}, nil
	}

	return &Server{
		status:       Status{State: StateReady, Root: abs},
		root:         abs,
		dir:          dir,
		fallbackHost: fallbackHost,
	}, nil
}

func checkCanaries(dir *anchoredDir) error {
	for _, rel := range canaryRelativePaths {
		st, err := dir.Lstat(rel)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("missing canary %q", rel)
			}
			return fmt.Errorf("canary %q: %w", rel, err)
		}
		if st.Mode()&fs.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return fmt.Errorf("canary %q is not a regular file", rel)
		}
	}
	return nil
}

// Status returns the construction-time readiness status.
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
