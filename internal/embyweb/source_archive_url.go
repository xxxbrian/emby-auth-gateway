package embyweb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"
)

// archiveURLSourceSpec configures remote .tar.gz archive acquisition.
// URL must be an absolute http(s) URL whose path ends with case-sensitive ".tar.gz".
type archiveURLSourceSpec struct {
	URL          string
	AllowHTTP    bool
	AllowPrivate bool
}

// archiveURLSource downloads a single prepared .tar.gz over HTTP(S) and extracts
// it with the same stream rules as the local archive source.
type archiveURLSource struct {
	rawURL       string
	u            *url.URL
	allowHTTP    bool
	allowPrivate bool
	deps         urlSourceDeps

	// Optional package-private test overrides; zero means production default.
	testMaxCompressed int64
	testMaxExpanded   int64
	testMaxPayload    int64
	testMaxHeaders    int
	testMaxNodes      int
}

// newArchiveURLSource validates the archive URL form and returns a package-private source.
// No network I/O occurs until acquire.
func newArchiveURLSource(spec archiveURLSourceSpec, deps urlSourceDeps) (*archiveURLSource, error) {
	u, err := parseArchiveURL(spec.URL, spec.AllowHTTP)
	if err != nil {
		return nil, err
	}
	if deps.Resolve == nil {
		deps.Resolve = defaultURLResolve
	}
	if deps.DialContext == nil {
		deps.DialContext = defaultURLDialContext
	}
	return &archiveURLSource{
		rawURL:       spec.URL,
		u:            u,
		allowHTTP:    spec.AllowHTTP,
		allowPrivate: spec.AllowPrivate,
		deps:         deps,
	}, nil
}

func (s *archiveURLSource) kind() string { return "archive" }

func (s *archiveURLSource) limits() archiveLimits {
	lim := archiveLimits{
		maxCompressed: maxArchiveCompressedBytes,
		maxExpanded:   maxArchiveExpandedBytes,
		maxPayload:    int64(maxTotalBytes),
		maxHeaders:    maxEntries + maxDirs,
		maxNodes:      maxEntries + maxDirs,
	}
	if s.testMaxCompressed > 0 {
		lim.maxCompressed = s.testMaxCompressed
	}
	if s.testMaxExpanded > 0 {
		lim.maxExpanded = s.testMaxExpanded
	}
	if s.testMaxPayload > 0 {
		lim.maxPayload = s.testMaxPayload
	}
	if s.testMaxHeaders > 0 {
		lim.maxHeaders = s.testMaxHeaders
	}
	if s.testMaxNodes > 0 {
		lim.maxNodes = s.testMaxNodes
	}
	return lim
}

func (s *archiveURLSource) acquire(ctx context.Context, tc *trustedCatalog, w *stagingWriter) error {
	if s == nil {
		return errors.New("archive source: nil source")
	}
	if tc == nil {
		return errors.New("archive source: nil catalog")
	}
	if w == nil {
		return errors.New("archive source: nil staging writer")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	lim := s.limits()

	ctx, cancel := context.WithTimeout(ctx, urlAcquireTimeout)
	defer cancel()

	host := s.u.Hostname()
	port := s.u.Port()
	if port == "" {
		if s.u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	addrs, err := s.resolveOnce(ctx, host)
	if err != nil {
		return err
	}

	client := s.buildClient(host, port, addrs)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.u.String(), nil)
	if err != nil {
		return fmt.Errorf("archive source: build request: %w", err)
	}
	// Retain original Host for virtual hosts / TLS SNI via transport.
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("archive source: GET: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("archive source: GET: status %d", resp.StatusCode)
	}

	// Reject transparent compression / non-identity content encodings.
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		return fmt.Errorf("archive source: forbidden Content-Encoding %q", ce)
	}
	for _, v := range resp.Header.Values("Content-Encoding") {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part != "" && !strings.EqualFold(part, "identity") {
				return fmt.Errorf("archive source: forbidden Content-Encoding %q", v)
			}
		}
	}

	// Content-Length, when present, must be within the compressed archive cap.
	if resp.ContentLength >= 0 {
		if resp.ContentLength > lim.maxCompressed {
			return fmt.Errorf("archive source: Content-Length %d exceeds limit %d", resp.ContentLength, lim.maxCompressed)
		}
	}

	err = extractArchiveStream(ctx, resp.Body, tc, w, lim)
	// Best-effort drain residual body after extract failure/success.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return err
}

func (s *archiveURLSource) resolveOnce(ctx context.Context, host string) ([]netip.Addr, error) {
	// IP literals need no DNS.
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.Zone() != "" {
			return nil, fmt.Errorf("archive source: zoned address %q forbidden", host)
		}
		if err := checkURLIPAllowed(ip, s.allowPrivate); err != nil {
			return nil, fmt.Errorf("archive source: %w", err)
		}
		return []netip.Addr{ip}, nil
	}

	rctx, cancel := context.WithTimeout(ctx, urlDNSTimeout)
	defer cancel()
	raw, err := s.deps.Resolve(rctx, host)
	if err != nil {
		return nil, fmt.Errorf("archive source: resolve %q: %w", host, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("archive source: resolve %q: empty answer", host)
	}

	seen := make(map[netip.Addr]struct{}, len(raw))
	out := make([]netip.Addr, 0, len(raw))
	for _, a := range raw {
		a = a.Unmap()
		if a.Zone() != "" {
			return nil, fmt.Errorf("archive source: resolve %q: zoned address forbidden", host)
		}
		if err := checkURLIPAllowed(a, s.allowPrivate); err != nil {
			// Any forbidden address rejects the whole answer (anti-rebinding).
			return nil, fmt.Errorf("archive source: resolve %q: %w", host, err)
		}
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("archive source: resolve %q: empty answer after dedupe", host)
	}
	return out, nil
}

func (s *archiveURLSource) buildClient(host, port string, addrs []netip.Addr) *http.Client {
	pinned := &urlPinnedDialer{
		host:  host,
		port:  port,
		addrs: append([]netip.Addr(nil), addrs...),
		dial:  s.deps.DialContext,
	}

	tr := &http.Transport{
		Proxy:                  nil, // never ProxyFromEnvironment
		DialContext:            pinned.DialContext,
		ForceAttemptHTTP2:      false,
		MaxIdleConns:           urlMaxIdleConns,
		MaxIdleConnsPerHost:    urlMaxIdleConnsPerHost,
		MaxConnsPerHost:        urlMaxConnsPerHost,
		IdleConnTimeout:        urlIdleConnTimeout,
		TLSHandshakeTimeout:    urlTLSTimeout,
		ResponseHeaderTimeout:  urlHeaderTimeout,
		ExpectContinueTimeout:  1 * time.Second,
		MaxResponseHeaderBytes: urlMaxResponseHeaderBytes,
		DisableCompression:     true,
	}
	if s.deps.TLSClientConfig != nil {
		tr.TLSClientConfig = s.deps.TLSClientConfig.Clone()
	}

	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("archive source: redirects forbidden")
		},
		Jar:     nil,
		Timeout: 0, // deadlines come from context
	}
}

// parseArchiveURL validates an absolute archive download URL.
// Path must end with case-sensitive ".tar.gz"; query/fragment/userinfo are rejected.
func parseArchiveURL(raw string, allowHTTP bool) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("archive source: archive URL is empty")
	}
	if len(raw) > urlMaxBaseBytes {
		return nil, fmt.Errorf("archive source: archive URL exceeds %d bytes", urlMaxBaseBytes)
	}
	if strings.TrimSpace(raw) != raw {
		return nil, errors.New("archive source: archive URL must not have leading/trailing whitespace")
	}
	if strings.ContainsAny(raw, "?#") {
		return nil, errors.New("archive source: archive URL must not contain query or fragment")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("archive source: parse archive URL: %w", err)
	}
	if !u.IsAbs() {
		return nil, errors.New("archive source: archive URL must be absolute")
	}
	if u.Opaque != "" {
		return nil, errors.New("archive source: archive URL must not be opaque")
	}
	if u.User != nil {
		return nil, errors.New("archive source: archive URL must not contain userinfo")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("archive source: archive URL must not contain query or fragment")
	}

	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "https":
		// production default
	case "http":
		if !allowHTTP {
			return nil, errors.New("archive source: http URL requires allowHTTP")
		}
	default:
		return nil, fmt.Errorf("archive source: scheme %q not allowed", u.Scheme)
	}
	u.Scheme = scheme

	if u.Host == "" {
		return nil, errors.New("archive source: archive URL host is empty")
	}
	if strings.Contains(u.Host, "%") {
		return nil, errors.New("archive source: archive URL host must not be percent-encoded")
	}

	host := u.Hostname()
	if host == "" {
		return nil, errors.New("archive source: archive URL hostname is empty")
	}
	if err := validateURLHostname(host); err != nil {
		// Re-prefix for archive diagnostics while reusing hostname rules.
		return nil, fmt.Errorf("archive source: %s", strings.TrimPrefix(err.Error(), "url source: "))
	}

	if u.Path == "" || !strings.HasPrefix(u.Path, "/") {
		return nil, errors.New("archive source: archive URL path must be absolute")
	}
	if !strings.HasSuffix(u.Path, archiveSuffix) {
		return nil, fmt.Errorf("archive source: archive URL path must end with %q", archiveSuffix)
	}
	if strings.Contains(u.RawPath, "%") {
		if unescapeAmbiguous(u.RawPath) {
			return nil, errors.New("archive source: archive URL path percent-encoding is ambiguous")
		}
	}
	if err := validateArchiveURLPath(u.Path); err != nil {
		return nil, err
	}

	out := &url.URL{
		Scheme: u.Scheme,
		Host:   u.Host,
		Path:   u.Path,
	}
	return out, nil
}

func validateArchiveURLPath(p string) error {
	if p == "" || !strings.HasPrefix(p, "/") {
		return errors.New("archive source: archive URL path must be absolute")
	}
	if !strings.HasSuffix(p, archiveSuffix) {
		return fmt.Errorf("archive source: archive URL path must end with %q", archiveSuffix)
	}
	if strings.ContainsAny(p, "\\\x00") {
		return errors.New("archive source: archive URL path contains forbidden characters")
	}
	// path.Clean collapses "." / ".." / duplicate slashes; require already-clean form.
	if path.Clean(p) != p {
		return errors.New("archive source: archive URL path is not clean")
	}
	// Reject empty / relative segments (including trailing slash forms already
	// excluded by the .tar.gz suffix check).
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return errors.New("archive source: archive URL path contains empty or relative segments")
		}
	}
	return nil
}
