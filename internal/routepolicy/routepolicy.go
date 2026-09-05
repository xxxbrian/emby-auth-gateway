// Package routepolicy selects an upstream endpoint key for proxied requests
// using method/path/transport rules with deterministic priority ordering.
//
// Rules are configuration data owned by the control plane (admin CRUD), the
// same ownership model as internal/pathpolicy. The package itself is pure:
// it evaluates a rule set against a request and returns the winning target
// endpoint key, or "" when no rule matches (callers fall back to the default
// endpoint).
package routepolicy

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/pathpolicy"
)

// Transports identify how the gateway forwards a client request upstream.
const (
	TransportAny       = ""          // rule matches every transport
	TransportHTTP      = "http"      // ordinary (non-WebSocket) request
	TransportWebSocket = "websocket" // WebSocket Upgrade request
)

// Rule routes matching requests to a named endpoint key.
type Rule struct {
	ID        string
	Method    string // "" or "*" means any method; otherwise exact (case-insensitive)
	Path      string // pathpolicy syntax: exact, trailing *, or /{id} segments
	Transport string // TransportAny, TransportHTTP, or TransportWebSocket
	Target    string // endpoint key the request should use
	Priority  int    // higher wins among matching enabled rules
	Enabled   bool
	Reason    string
	// Updated is populated from storage when available (optimistic concurrency).
	Updated time.Time
}

// Request is the subset of an inbound proxy request used for routing.
type Request struct {
	Method    string
	Path      string
	Transport string // TransportHTTP or TransportWebSocket; "" is treated as HTTP
}

// Valid reports whether r can be persisted as a route rule.
func (r Rule) Valid() error {
	if strings.TrimSpace(r.Target) == "" {
		return fmt.Errorf("target endpoint key is required")
	}
	if strings.TrimSpace(r.Path) == "" {
		return fmt.Errorf("path is required")
	}
	switch strings.ToLower(strings.TrimSpace(r.Transport)) {
	case TransportAny, TransportHTTP, TransportWebSocket:
	default:
		return fmt.Errorf("transport must be one of %q, %q, or empty", TransportHTTP, TransportWebSocket)
	}
	return nil
}

// NormalizedIdentity mirrors pathpolicy normalization for storage dedupe.
func NormalizedIdentity(r Rule) (string, string, string) {
	return normalizeMethod(r.Method), strings.ToLower(pathpolicyNormalizedPath(r.Path)), normalizeTransport(r.Transport)
}

// Select returns the target endpoint key of the best matching enabled rule for
// the request, or "" when no rule matches. Matching is deterministic:
// deny-style precedence does not apply; higher priority wins, and ties break
// toward the more specific transport, then the more specific method, then the
// longer path pattern, then rule identity.
func Select(rules []Rule, req Request) string {
	rule, ok := selectRule(rules, req)
	if !ok {
		return ""
	}
	return rule.Target
}

// SelectRule is Select with the winning rule returned (for preview/audit).
func SelectRule(rules []Rule, req Request) (Rule, bool) {
	return selectRule(rules, req)
}

func selectRule(rules []Rule, req Request) (Rule, bool) {
	transport := normalizeTransport(req.Transport)
	if transport == "" {
		transport = TransportHTTP
	}
	method := strings.TrimSpace(req.Method)
	matched := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if !methodMatches(r.Method, method) || !pathMatches(r.Path, req.Path) || !transportMatches(r.Transport, transport) {
			continue
		}
		matched = append(matched, r)
	}
	if len(matched) == 0 {
		return Rule{}, false
	}
	sort.SliceStable(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		as, bs := transportSpecificity(a.Transport), transportSpecificity(b.Transport)
		if as != bs {
			return as > bs
		}
		am, bm := methodSpecificity(a.Method), methodSpecificity(b.Method)
		if am != bm {
			return am > bm
		}
		ap, bp := patternSpecificity(a.Path), patternSpecificity(b.Path)
		if ap != bp {
			return ap > bp
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.ID < b.ID
	})
	return matched[0], true
}

func methodMatches(rule, request string) bool {
	rule = strings.TrimSpace(rule)
	return rule == "" || rule == "*" || strings.EqualFold(rule, request)
}

func pathMatches(pattern, path string) bool {
	// Reuse pathpolicy's exact/terminal-* /{id} single-segment matcher.
	return pathpolicy.MatchPath(pattern, path)
}

func transportMatches(rule, request string) bool {
	rule = normalizeTransport(rule)
	return rule == TransportAny || rule == request
}

func normalizeTransport(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case TransportHTTP, TransportWebSocket:
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return TransportAny
	}
}

func normalizeMethod(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "*" {
		return "*"
	}
	return strings.ToUpper(value)
}

func pathpolicyNormalizedPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "/" {
		return value
	}
	return strings.TrimRight(value, "/")
}

// transportSpecificity: an exact transport rule beats an any-transport rule
// when both match, so a WebSocket-only route can shadow a generic route.
func transportSpecificity(value string) int {
	if normalizeTransport(value) != TransportAny {
		return 1
	}
	return 0
}

// methodSpecificity: an exact method beats a wildcard method at equal priority.
func methodSpecificity(value string) int {
	value = strings.TrimSpace(value)
	if value != "" && value != "*" {
		return 1
	}
	return 0
}

// patternSpecificity: a longer pattern is more specific; ties are resolved by
// literal comparison in selectRule.
func patternSpecificity(value string) int {
	return len(strings.TrimSpace(value))
}
