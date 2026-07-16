package embyweb

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// Host placeholder rewritten at serve time for selected JS assets only.
// Disk release contents are never modified.
var injectHostPlaceholder = []byte("mb3admin.com")

// hostInjectPaths are web-root-relative paths that receive host injection.
var hostInjectPaths = map[string]struct{}{
	"modules/emby-apiclient/connectionmanager.js": {},
	"embypremiere/embypremiere.js":                {},
}

func needsHostInject(assetPath string) bool {
	_, ok := hostInjectPaths[assetPath]
	return ok
}

// injectHostForRequest prefers a valid r.Host; falls back to a valid configured
// public host. Invalid or empty values yield "" so rewrite is skipped.
func injectHostForRequest(r *http.Request, fallbackHost string) string {
	if r != nil {
		if host := strings.TrimSpace(r.Host); validInjectHost(host) {
			return host
		}
	}
	if host := strings.TrimSpace(fallbackHost); validInjectHost(host) {
		return host
	}
	return ""
}

// hostFromPublicURL extracts host[:port] from GATEWAY_PUBLIC_URL-style base URLs.
// The result is returned only when it passes validInjectHost.
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
		} else {
			return ""
		}
	}
	host := strings.TrimSpace(u.Host)
	if !validInjectHost(host) {
		return ""
	}
	return host
}

// validInjectHost reports whether host is safe for placeholder rewrite:
// hostname or IP, optional :port. Rejects empty, spaces, quotes, slashes,
// userinfo (@), control characters, and other non-host forms.
func validInjectHost(host string) bool {
	if host == "" {
		return false
	}
	// Must already be trimmed; reject surrounding or interior whitespace.
	if strings.TrimSpace(host) != host {
		return false
	}
	for _, r := range host {
		if r < 0x20 || r == 0x7f || unicode.IsSpace(r) {
			return false
		}
		switch r {
		case '"', '\'', '`', '/', '\\', '@', '?', '#', '<', '>', '{', '}', '|', '^', ',', ';', '%', '=':
			return false
		}
	}

	hostname, port, ok := splitInjectHostPort(host)
	if !ok {
		return false
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return validInjectHostname(hostname)
}

// splitInjectHostPort splits host or host:port (including bracketed IPv6).
func splitInjectHostPort(host string) (hostname, port string, ok bool) {
	// Bracketed IPv6: [::1] or [::1]:8080
	if strings.HasPrefix(host, "[") {
		h, p, err := net.SplitHostPort(host)
		if err == nil {
			return h, p, h != ""
		}
		// No port: must be exactly [ipv6]
		if !strings.HasSuffix(host, "]") {
			return "", "", false
		}
		inner := host[1 : len(host)-1]
		if inner == "" || net.ParseIP(inner) == nil {
			return "", "", false
		}
		return inner, "", true
	}

	// host:port (single colon) or bare hostname/IPv4
	if strings.Count(host, ":") == 0 {
		return host, "", true
	}
	if strings.Count(host, ":") == 1 {
		h, p, err := net.SplitHostPort(host)
		if err != nil || h == "" || p == "" {
			return "", "", false
		}
		return h, p, true
	}
	// Multiple colons without brackets: only bare IPv6 (no port) is accepted.
	if net.ParseIP(host) != nil {
		return host, "", true
	}
	return "", "", false
}

func validInjectHostname(hostname string) bool {
	if hostname == "" || len(hostname) > 253 {
		return false
	}
	if ip := net.ParseIP(hostname); ip != nil {
		return true
	}
	// DNS hostname: labels of alnum/hyphen, no leading/trailing hyphen per label.
	if hostname[0] == '.' || hostname[len(hostname)-1] == '.' {
		return false
	}
	for _, label := range strings.Split(hostname, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
			isDigit := c >= '0' && c <= '9'
			if !isAlpha && !isDigit && c != '-' {
				return false
			}
		}
	}
	return true
}

// rewriteHostPlaceholder replaces mb3admin.com with host. Empty or invalid host
// leaves data unchanged so the on-wire body keeps the original placeholder.
func rewriteHostPlaceholder(data []byte, host string) []byte {
	host = strings.TrimSpace(host)
	if !validInjectHost(host) || !bytes.Contains(data, injectHostPlaceholder) {
		return data
	}
	return bytes.ReplaceAll(data, injectHostPlaceholder, []byte(host))
}

func etagForBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}
