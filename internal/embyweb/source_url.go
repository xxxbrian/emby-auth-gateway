package embyweb

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/sync/errgroup"
)

// Prepared static-tree URL acquisition bounds (Phase 3 contract).
const (
	urlMaxBaseBytes           = 2048
	urlDNSTimeout             = 10 * time.Second
	urlDialTimeout            = 10 * time.Second
	urlTLSTimeout             = 15 * time.Second
	urlHeaderTimeout          = 15 * time.Second
	urlPerFileTimeout         = 5 * time.Minute
	urlAcquireTimeout         = 30 * time.Minute
	urlMaxConcurrency         = 8
	urlMaxResponseHeaderBytes = 64 << 10 // 64 KiB
	urlIdleConnTimeout        = 90 * time.Second
	urlMaxIdleConns           = 32
	urlMaxIdleConnsPerHost    = 8
	urlMaxConnsPerHost        = 8
)

// urlSourceSpec configures a prepared static-tree URL acquisition source.
// BaseURL must be an absolute slash-terminated base; AllowHTTP/AllowPrivate are
// independent development flags (HTTP to private destinations requires both).
type urlSourceSpec struct {
	BaseURL      string
	AllowHTTP    bool
	AllowPrivate bool
}

// urlSourceDeps are package-private test hooks. Production leaves all fields nil.
// Resolver/dial/TLS injection must not be exported.
type urlSourceDeps struct {
	// Resolve looks up host and returns IP addresses. When nil, net.DefaultResolver
	// is used with a 10s bound. IP-literal hosts never call Resolve.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// DialContext dials network/address (literal host:port). When nil, a 10s
	// net.Dialer is used. The transport never dials the original hostname.
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
	// TLSClientConfig, when non-nil, is cloned onto the transport (tests only).
	// Production leaves this nil so system roots and normal cert verification apply.
	TLSClientConfig *tls.Config
}

// urlSource fetches every trusted catalog path from a prepared static-tree base URL.
type urlSource struct {
	base         *url.URL // validated absolute base; Path ends with "/"
	rawBase      string
	allowHTTP    bool
	allowPrivate bool
	deps         urlSourceDeps
}

// newURLSource validates spec and returns a package-private acquisitionSource.
func newURLSource(spec urlSourceSpec, deps urlSourceDeps) (*urlSource, error) {
	base, err := parseURLBase(spec.BaseURL, spec.AllowHTTP)
	if err != nil {
		return nil, err
	}
	if deps.Resolve == nil {
		deps.Resolve = defaultURLResolve
	}
	if deps.DialContext == nil {
		deps.DialContext = defaultURLDialContext
	}
	return &urlSource{
		base:         base,
		rawBase:      spec.BaseURL,
		allowHTTP:    spec.AllowHTTP,
		allowPrivate: spec.AllowPrivate,
		deps:         deps,
	}, nil
}

func (s *urlSource) kind() string { return "url" }

func (s *urlSource) acquire(ctx context.Context, tc *trustedCatalog, w *stagingWriter) error {
	if s == nil {
		return errors.New("url source: nil source")
	}
	if tc == nil {
		return errors.New("url source: nil catalog")
	}
	if w == nil {
		return errors.New("url source: nil staging writer")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Catalog-bounded total payload (same 256 MiB ceiling as the reader/installer).
	var total int64
	for _, e := range tc.Catalog.Entries {
		if e.Size < 0 || e.Size > maxFileBytes {
			return fmt.Errorf("url source: entry %q size out of bounds: %d", e.Path, e.Size)
		}
		total += e.Size
		if total > maxTotalBytes {
			return fmt.Errorf("url source: catalog total size exceeds %d bytes", maxTotalBytes)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, urlAcquireTimeout)
	defer cancel()

	host := s.base.Hostname()
	port := s.base.Port()
	if port == "" {
		if s.base.Scheme == "https" {
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

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(urlMaxConcurrency)
	for _, e := range tc.Catalog.Entries {
		e := e
		g.Go(func() error {
			return s.fetchOne(gctx, client, e, w)
		})
	}
	return g.Wait()
}

func (s *urlSource) resolveOnce(ctx context.Context, host string) ([]netip.Addr, error) {
	// IP literals need no DNS.
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.Zone() != "" {
			return nil, fmt.Errorf("url source: zoned address %q forbidden", host)
		}
		if err := checkURLIPAllowed(ip, s.allowPrivate); err != nil {
			return nil, err
		}
		return []netip.Addr{ip}, nil
	}

	rctx, cancel := context.WithTimeout(ctx, urlDNSTimeout)
	defer cancel()
	raw, err := s.deps.Resolve(rctx, host)
	if err != nil {
		return nil, fmt.Errorf("url source: resolve %q: %w", host, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("url source: resolve %q: empty answer", host)
	}

	seen := make(map[netip.Addr]struct{}, len(raw))
	out := make([]netip.Addr, 0, len(raw))
	for _, a := range raw {
		a = a.Unmap()
		if a.Zone() != "" {
			return nil, fmt.Errorf("url source: resolve %q: zoned address forbidden", host)
		}
		if err := checkURLIPAllowed(a, s.allowPrivate); err != nil {
			// Any forbidden address rejects the whole answer (anti-rebinding).
			return nil, fmt.Errorf("url source: resolve %q: %w", host, err)
		}
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("url source: resolve %q: empty answer after dedupe", host)
	}
	return out, nil
}

func (s *urlSource) buildClient(host, port string, addrs []netip.Addr) *http.Client {
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
		// Leave TLSClientConfig nil in production for normal cert verification.
	}
	if s.deps.TLSClientConfig != nil {
		tr.TLSClientConfig = s.deps.TLSClientConfig.Clone()
	}

	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("url source: redirects forbidden")
		},
		Jar:     nil,
		Timeout: 0, // deadlines come from context
	}
}

func (s *urlSource) fetchOne(ctx context.Context, client *http.Client, e installEntry, w *stagingWriter) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fileURL, err := joinURLBasePath(s.base, e.Path)
	if err != nil {
		return err
	}

	fileCtx, cancel := context.WithTimeout(ctx, urlPerFileTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fileCtx, http.MethodGet, fileURL.String(), nil)
	if err != nil {
		return fmt.Errorf("url source: build request for %q: %w", e.Path, err)
	}
	// Retain original Host (from URL) for virtual hosts / TLS SNI via transport.
	req.Header.Set("Accept-Encoding", "identity")
	// Do not set Host manually; http.Request derives it from URL so SNI matches.

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("url source: GET %q: %w", e.Path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("url source: GET %q: status %d", e.Path, resp.StatusCode)
	}

	// Reject transparent compression / non-identity content encodings.
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		return fmt.Errorf("url source: GET %q: forbidden Content-Encoding %q", e.Path, ce)
	}
	// Multi-value encodings: any non-empty non-identity token is forbidden.
	for _, v := range resp.Header.Values("Content-Encoding") {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part != "" && !strings.EqualFold(part, "identity") {
				return fmt.Errorf("url source: GET %q: forbidden Content-Encoding %q", e.Path, v)
			}
		}
	}

	if resp.ContentLength >= 0 && resp.ContentLength != e.Size {
		return fmt.Errorf("url source: GET %q: Content-Length %d != catalog size %d", e.Path, resp.ContentLength, e.Size)
	}

	// stagingWriter reads exactly size+1 (hash/sync/exclusive create).
	if err := w.writeFile(e.Path, resp.Body); err != nil {
		return fmt.Errorf("url source: write %q: %w", e.Path, err)
	}
	// Ensure body is closed (defer) and discard any residual best-effort.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1))
	return nil
}

// ---------------------------------------------------------------------------
// URL grammar
// ---------------------------------------------------------------------------

func parseURLBase(raw string, allowHTTP bool) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("url source: base URL is empty")
	}
	if len(raw) > urlMaxBaseBytes {
		return nil, fmt.Errorf("url source: base URL exceeds %d bytes", urlMaxBaseBytes)
	}
	if strings.TrimSpace(raw) != raw {
		return nil, errors.New("url source: base URL must not have leading/trailing whitespace")
	}
	// Reject query/fragment markers in the raw form (including empty "?") before parse.
	if strings.ContainsAny(raw, "?#") {
		return nil, errors.New("url source: base URL must not contain query or fragment")
	}
	// Opaque forms and missing scheme separators.
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("url source: parse base URL: %w", err)
	}
	if u.IsAbs() == false {
		return nil, errors.New("url source: base URL must be absolute")
	}
	if u.Opaque != "" {
		return nil, errors.New("url source: base URL must not be opaque")
	}
	if u.User != nil {
		return nil, errors.New("url source: base URL must not contain userinfo")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("url source: base URL must not contain query or fragment")
	}
	if raw != u.String() && !urlBaseStringEquivalent(raw, u) {
		// Allow only trivial Parse normalizations we already constrain (e.g. empty port).
		// Prefer rejecting surprising rewrites.
		if u.Scheme == "" || u.Host == "" {
			return nil, errors.New("url source: base URL is not a usable absolute URL")
		}
	}

	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "https":
		// production default
	case "http":
		if !allowHTTP {
			return nil, errors.New("url source: http base requires allowHTTP")
		}
	default:
		return nil, fmt.Errorf("url source: scheme %q not allowed", u.Scheme)
	}
	u.Scheme = scheme

	if u.Host == "" {
		return nil, errors.New("url source: base URL host is empty")
	}
	if strings.Contains(u.Host, "%") {
		return nil, errors.New("url source: base URL host must not be percent-encoded")
	}

	host := u.Hostname()
	if host == "" {
		return nil, errors.New("url source: base URL hostname is empty")
	}
	if err := validateURLHostname(host); err != nil {
		return nil, err
	}

	// Explicit path ending with "/".
	if u.Path == "" || !strings.HasSuffix(u.Path, "/") {
		return nil, errors.New("url source: base URL path must be non-empty and end with '/'")
	}
	if strings.Contains(u.RawPath, "%") {
		// Percent-encoded path segments can hide ".." / separators; reject ambiguity.
		// Catalog joins use validated relative paths only; base path must be plain.
		if unescapeAmbiguous(u.RawPath) {
			return nil, errors.New("url source: base URL path percent-encoding is ambiguous")
		}
	}
	// Path must not escape via ".." segments.
	if err := validateURLBasePath(u.Path); err != nil {
		return nil, err
	}

	// Drop any residual user/query/fragment and normalize for joins.
	out := &url.URL{
		Scheme: u.Scheme,
		Host:   u.Host,
		Path:   u.Path,
	}
	return out, nil
}

func urlBaseStringEquivalent(raw string, u *url.URL) bool {
	// url.Parse lowercases scheme in String() inconsistently across versions;
	// compare structural fields we care about.
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Scheme, u.Scheme) &&
		parsed.Host == u.Host &&
		parsed.Path == u.Path &&
		parsed.Opaque == "" &&
		parsed.User == nil &&
		parsed.RawQuery == "" &&
		parsed.Fragment == ""
}

func validateURLHostname(host string) error {
	// ASCII only; no spaces/controls; no percent.
	if strings.Contains(host, "%") {
		return errors.New("url source: hostname must not be percent-encoded")
	}
	for _, r := range host {
		if r > unicode.MaxASCII || r < 0x21 || r == 0x7f {
			return errors.New("url source: hostname must be printable ASCII")
		}
	}
	// IP literal is fine (brackets already stripped by Hostname()).
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	// DNS name: labels of LDH, dots, no leading/trailing dot, no empty labels.
	if host[0] == '.' || host[len(host)-1] == '.' || strings.Contains(host, "..") {
		return errors.New("url source: hostname is not a valid DNS name")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return errors.New("url source: hostname label invalid")
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-'
			if !ok {
				return errors.New("url source: hostname must be ASCII LDH labels")
			}
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("url source: hostname label must not start/end with '-'")
		}
	}
	return nil
}

func validateURLBasePath(p string) error {
	if p == "" || !strings.HasPrefix(p, "/") || !strings.HasSuffix(p, "/") {
		return errors.New("url source: base path must be absolute and end with '/'")
	}
	// Reject backslash and NUL.
	if strings.ContainsAny(p, "\\\x00") {
		return errors.New("url source: base path contains forbidden characters")
	}
	// Clean without the trailing slash for comparison; require no ".." escape.
	trim := strings.TrimSuffix(p, "/")
	if trim == "" {
		// path is "/"
		return nil
	}
	cleaned := path.Clean(trim)
	if cleaned != trim {
		return errors.New("url source: base path is not clean")
	}
	for _, seg := range strings.Split(strings.TrimPrefix(trim, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return errors.New("url source: base path contains empty or relative segments")
		}
	}
	return nil
}

func unescapeAmbiguous(rawPath string) bool {
	// Treat any percent-encoding of "/", "\", ".", or NUL as ambiguous.
	lower := strings.ToLower(rawPath)
	return strings.Contains(lower, "%2f") ||
		strings.Contains(lower, "%5c") ||
		strings.Contains(lower, "%2e") ||
		strings.Contains(lower, "%00")
}

// joinURLBasePath resolves a validated catalog relative path under base without
// allowing path-cleaning escape outside the base prefix.
//
// The result sets Path to the joined absolute path (which may contain literal
// '?', '#', or '%' bytes from trusted catalog filenames). RawQuery and Fragment
// are always empty so http.NewRequest uses EscapedPath: those bytes are
// percent-encoded into the request path and never become query/fragment.
//
// Literal ".." inside a single filename segment (e.g. "foo..js") is allowed:
// validAssetPath already rejects the ".." path segment and path.Clean does not
// rewrite such names. Escape checks use clean path equality and base-prefix
// invariants only — never a raw substring search for ".." or "?#".
func joinURLBasePath(base *url.URL, rel string) (*url.URL, error) {
	if base == nil {
		return nil, errors.New("url source: nil base")
	}
	if !validAssetPath(rel) {
		return nil, fmt.Errorf("url source: invalid catalog path %q", rel)
	}
	if !strings.HasSuffix(base.Path, "/") {
		return nil, errors.New("url source: internal base path missing trailing '/'")
	}
	// Base path is validated at construction; re-assert trailing-slash + clean form.
	if err := validateURLBasePath(base.Path); err != nil {
		return nil, err
	}

	full := base.Path + rel
	cleanFull := path.Clean(full)
	// Joined path must already be clean. path.Clean collapses "." / ".." segments
	// and duplicate slashes, but leaves literal ".." / "?" / "#" inside a
	// filename segment unchanged.
	if cleanFull != full {
		return nil, fmt.Errorf("url source: path %q is not clean under base", rel)
	}

	cleanBase := path.Clean(strings.TrimSuffix(base.Path, "/"))
	if cleanBase == "." || cleanBase == "" {
		cleanBase = "/"
	}
	// Cleaned result must remain under the cleaned base prefix.
	if cleanBase == "/" {
		// Root base: require a non-root absolute path (rel is non-empty via validAssetPath).
		if !strings.HasPrefix(cleanFull, "/") || cleanFull == "/" {
			return nil, fmt.Errorf("url source: path %q escapes base", rel)
		}
	} else if cleanFull != cleanBase && !strings.HasPrefix(cleanFull, cleanBase+"/") {
		return nil, fmt.Errorf("url source: path %q escapes base", rel)
	}

	out := &url.URL{
		Scheme: base.Scheme,
		Host:   base.Host,
		// Path holds the decoded path form (may include literal ? # %).
		// Empty RawPath forces EscapedPath to percent-encode from Path.
		Path:    full,
		RawPath: "",
		// Never inherit query/fragment from a mutated base; request target is path-only.
		RawQuery: "",
		Fragment: "",
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// DNS / SSRF policy
// ---------------------------------------------------------------------------

// privateOptInPrefixes are denied by default and permitted only with allowPrivate:
// exactly loopback, RFC1918, ULA, and CGNAT. No other special-purpose ranges.
var privateOptInPrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),     // RFC1918
	netip.MustParsePrefix("172.16.0.0/12"),  // RFC1918
	netip.MustParsePrefix("192.168.0.0/16"), // RFC1918
	netip.MustParsePrefix("100.64.0.0/10"),  // CGNAT RFC6598
	netip.MustParsePrefix("127.0.0.0/8"),    // IPv4 loopback
	netip.MustParsePrefix("::1/128"),        // IPv6 loopback
	netip.MustParsePrefix("fc00::/7"),       // ULA RFC4193
}

// alwaysForbiddenPrefixes are never permitted, even with allowPrivate.
// Sourced from IANA IPv4/IPv6 Special-Purpose Address Registries (and RFCs that
// allocate globally-routable-looking special ranges). Private opt-in ranges are
// intentionally absent here.
//
// Note: several IPv6 assignments under 2001::/23 (AMT, AS112-v6, ORCHID, DRIP)
// are covered by that coarser prefix; finer prefixes are listed when they aid
// review or sit outside 2001::/23.
var alwaysForbiddenPrefixes = []netip.Prefix{
	// IPv4 — IANA special-purpose / reserved
	netip.MustParsePrefix("0.0.0.0/8"),       // this network / unspecified
	netip.MustParsePrefix("169.254.0.0/16"),  // link-local (includes 169.254.169.254 metadata)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1 documentation
	netip.MustParsePrefix("192.31.196.0/24"), // AS112-v4 (RFC7535)
	netip.MustParsePrefix("192.52.193.0/24"), // AMT (RFC7450)
	netip.MustParsePrefix("192.88.99.0/24"),  // 6to4 relay anycast (deprecated, RFC7526)
	netip.MustParsePrefix("192.175.48.0/24"), // Direct Delegation AS112 Service (RFC7534)
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking RFC2544
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2 documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3 documentation
	netip.MustParsePrefix("224.0.0.0/4"),     // multicast
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved for future use / class E

	// IPv6 — IANA special-purpose / reserved / transition / documentation
	netip.MustParsePrefix("::/128"),            // unspecified
	netip.MustParsePrefix("64:ff9b::/96"),      // NAT64 well-known prefix
	netip.MustParsePrefix("64:ff9b:1::/48"),    // local-use NAT64
	netip.MustParsePrefix("100::/64"),          // discard-only RFC6666
	netip.MustParsePrefix("2001::/23"),         // IETF protocol assignments (covers AMT/AS112/ORCHID/DRIP sub-assignments)
	netip.MustParsePrefix("2001:2::/48"),       // benchmarking (also under 2001::/23; explicit for review)
	netip.MustParsePrefix("2001:db8::/32"),     // documentation RFC3849
	netip.MustParsePrefix("2002::/16"),         // 6to4 transition
	netip.MustParsePrefix("2620:4f:8000::/48"), // Direct Delegation AS112 Service (RFC7534)
	netip.MustParsePrefix("3fff::/20"),         // documentation RFC9637
	netip.MustParsePrefix("5f00::/16"),         // SRv6 SIDs RFC9602
	netip.MustParsePrefix("fe80::/10"),         // link-local unicast
	netip.MustParsePrefix("fec0::/10"),         // deprecated site-local (RFC3879)
	netip.MustParsePrefix("ff00::/8"),          // multicast
}

// explicitMetadataAddrs are always denied (defense-in-depth beyond link-local).
var explicitMetadataAddrs = []netip.Addr{
	netip.MustParseAddr("169.254.169.254"),
	netip.MustParseAddr("169.254.170.2"), // AWS ECS task metadata common addr
}

func checkURLIPAllowed(ip netip.Addr, allowPrivate bool) error {
	if !ip.IsValid() {
		return errors.New("invalid IP address")
	}
	ip = ip.Unmap()
	if ip.Zone() != "" {
		return fmt.Errorf("address %s has zone identifier", ip)
	}
	for _, m := range explicitMetadataAddrs {
		if ip == m {
			return fmt.Errorf("address %s is forbidden (metadata)", ip)
		}
	}
	for _, p := range alwaysForbiddenPrefixes {
		if p.Contains(ip) {
			return fmt.Errorf("address %s is forbidden", ip)
		}
	}
	// allowPrivate permits only the explicit private-opt-in set (loopback /
	// RFC1918 / ULA / CGNAT). Those addresses are accepted here before the
	// global-unicast fail-closed check so loopback is not rejected by it.
	for _, p := range privateOptInPrefixes {
		if p.Contains(ip) {
			if !allowPrivate {
				return fmt.Errorf("address %s is private/loopback/CGNAT (requires allowPrivate)", ip)
			}
			return nil
		}
	}
	// Fail closed: after explicit tables, require global unicast. This rejects
	// non-global leftovers (and future special forms) without re-opening the
	// private opt-in set already handled above.
	//
	// Note: netip.IsGlobalUnicast is intentionally loose (true for RFC1918/ULA
	// and many special-purpose blocks); explicit tables remain authoritative for
	// those. This check only denies addresses that are not global unicast at all.
	if !ip.IsGlobalUnicast() {
		return fmt.Errorf("address %s is not global unicast", ip)
	}
	return nil
}

func defaultURLResolve(ctx context.Context, host string) ([]netip.Addr, error) {
	// Prefer LookupNetIP when available (Go 1.18+).
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	return ips, nil
}

func defaultURLDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d := net.Dialer{Timeout: urlDialTimeout}
	return d.DialContext(ctx, network, address)
}

// urlPinnedDialer dials only pre-validated literal IPs. It accepts DialContext
// calls solely for the original hostname:port and never performs a second DNS lookup.
type urlPinnedDialer struct {
	host  string
	port  string
	addrs []netip.Addr
	dial  func(ctx context.Context, network, address string) (net.Conn, error)

	mu   sync.Mutex
	next int
}

func (d *urlPinnedDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("url source: dial split %q: %w", address, err)
	}
	// Accept only the original hostname (or its IP-literal form) and port.
	if port != d.port {
		return nil, fmt.Errorf("url source: unexpected dial port %q (want %q)", port, d.port)
	}
	if !urlDialHostMatches(host, d.host) {
		return nil, fmt.Errorf("url source: unexpected dial host %q (want %q)", host, d.host)
	}

	// Round-robin across validated literals; no hostname dial.
	n := len(d.addrs)
	if n == 0 {
		return nil, errors.New("url source: no validated addresses to dial")
	}
	d.mu.Lock()
	start := d.next % n
	d.next++
	d.mu.Unlock()

	var lastErr error
	for i := 0; i < n; i++ {
		addr := d.addrs[(start+i)%n]
		literal := net.JoinHostPort(addr.String(), port)
		conn, err := d.dial(ctx, network, literal)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("url source: dial failed")
	}
	return nil, lastErr
}

func urlDialHostMatches(got, want string) bool {
	if strings.EqualFold(got, want) {
		return true
	}
	// IP literal forms: strip brackets if present.
	g := strings.TrimPrefix(strings.TrimSuffix(got, "]"), "[")
	w := strings.TrimPrefix(strings.TrimSuffix(want, "]"), "[")
	ga, gerr := netip.ParseAddr(g)
	wa, werr := netip.ParseAddr(w)
	if gerr == nil && werr == nil {
		return ga.Unmap() == wa.Unmap()
	}
	return false
}
