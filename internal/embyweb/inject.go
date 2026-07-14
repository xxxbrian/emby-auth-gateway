package embyweb

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
)

// Host placeholder rewritten at serve time for selected JS assets only.
// Disk release contents are never modified.
var injectHostPlaceholder = []byte("mb3admin.co")

// hostInjectPaths are catalog-relative paths that receive host injection.
var hostInjectPaths = map[string]struct{}{
	"modules/emby-apiclient/connectionmanager.js": {},
	"embypremiere/embypremiere.js":                {},
}

func needsHostInject(assetPath string) bool {
	_, ok := hostInjectPaths[assetPath]
	return ok
}

// injectHostForRequest prefers r.Host; falls back to the configured public host.
func injectHostForRequest(r *http.Request, fallbackHost string) string {
	if r != nil {
		if host := strings.TrimSpace(r.Host); host != "" {
			return host
		}
	}
	return strings.TrimSpace(fallbackHost)
}

// hostFromPublicURL extracts host[:port] from GATEWAY_PUBLIC_URL-style base URLs.
func hostFromPublicURL(publicURL string) string {
	publicURL = strings.TrimSpace(publicURL)
	if publicURL == "" {
		return ""
	}
	u, err := url.Parse(publicURL)
	if err != nil || u.Host == "" {
		// Accept bare host or host/path without scheme.
		if !strings.Contains(publicURL, "://") {
			u, err = url.Parse("http://" + publicURL)
			if err != nil || u.Host == "" {
				return ""
			}
			return u.Host
		}
		return ""
	}
	return u.Host
}

// rewriteHostPlaceholder replaces mb3admin.co with host. Empty host leaves data unchanged.
func rewriteHostPlaceholder(data []byte, host string) []byte {
	host = strings.TrimSpace(host)
	if host == "" || !bytes.Contains(data, injectHostPlaceholder) {
		return data
	}
	return bytes.ReplaceAll(data, injectHostPlaceholder, []byte(host))
}

func etagForBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}
