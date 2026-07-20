package gateway

import (
	"fmt"
	"net/http"
	"strings"
)

type responseHeaderPlan struct {
	header http.Header
}

func buildResponseHeaderPlan(dst, src http.Header, rel string, session *Session, upstream upstreamRequestSnapshot, gatewayToken, publicGatewayBase, gatewayServerID string, resource resourceRouteKind, status int, projection responseProjection) (*responseHeaderPlan, error) {
	header := gatewayOwnedResponseHeaders(dst)
	if resource != resourceRouteNone {
		header.Del("Cache-Control")
	}
	upstreamHeader := src.Clone()
	sanitizeHopHeaders(upstreamHeader)
	for name, values := range upstreamHeader {
		if strings.EqualFold(name, "Content-Length") || isResponseCredentialHeader(name) || strings.EqualFold(name, "Cookie") {
			continue
		}
		if strings.HasPrefix(strings.ToLower(name), "access-control-") && header.Get(name) != "" {
			continue
		}
		if strings.EqualFold(name, "Cache-Control") && header.Get("Cache-Control") != "" {
			continue
		}
		for _, value := range values {
			if strings.EqualFold(name, "Location") || strings.EqualFold(name, "Content-Location") {
				value = rewriteResponseLocation(value, rel, session, upstream, gatewayToken, publicGatewayBase, gatewayServerID)
				if err := validateCredentialSafeHeaderValue(value, upstream.token); err != nil {
					return nil, fmt.Errorf("header %s: %w", name, err)
				}
			} else {
				if err := validateCredentialSafeHeaderValue(value, upstream.token); err != nil {
					return nil, fmt.Errorf("header %s: %w", name, err)
				}
				if projection.kind == responseProjectionLegacyCompatibility {
					value = rewriteLegacyHeaderIdentity(value, session, upstream, gatewayServerID)
				}
			}
			if strings.EqualFold(name, "Vary") {
				mergeVaryValue(header, value)
			} else {
				header.Add(name, value)
			}
		}
	}
	applyResourceCachePolicy(header, resource, status)
	for name, values := range header {
		for _, value := range values {
			if err := validateCredentialSafeHeaderValue(value, upstream.token); err != nil {
				return nil, fmt.Errorf("header %s: %w", name, err)
			}
		}
	}
	return &responseHeaderPlan{header: header}, nil
}

func rewriteLegacyHeaderIdentity(value string, session *Session, upstream upstreamRequestSnapshot, gatewayServerID string) string {
	if session == nil {
		return value
	}
	if upstream.userID != "" {
		value = strings.ReplaceAll(value, upstream.userID, session.SyntheticUserID)
	}
	if upstream.serverID != "" {
		value = strings.ReplaceAll(value, upstream.serverID, gatewayServerID)
	}
	return value
}

func (p *responseHeaderPlan) Commit(dst http.Header) {
	clearHeader(dst)
	if p == nil {
		return
	}
	for name, values := range p.header {
		dst[name] = append([]string(nil), values...)
	}
}

func (p *responseHeaderPlan) Header() http.Header {
	if p == nil {
		return nil
	}
	return p.header
}

func resetProjectionFailureHeaders(dst http.Header) {
	preserved := gatewayOwnedResponseHeaders(dst)
	clearHeader(dst)
	for name, values := range preserved {
		if strings.EqualFold(name, "Cache-Control") {
			continue
		}
		dst[name] = append([]string(nil), values...)
	}
	dst.Set("Cache-Control", "no-store")
}

func gatewayOwnedResponseHeaders(src http.Header) http.Header {
	result := make(http.Header)
	for name, values := range src {
		if strings.HasPrefix(strings.ToLower(name), "access-control-") || strings.EqualFold(name, "Vary") || strings.EqualFold(name, "Cache-Control") {
			result[name] = append([]string(nil), values...)
		}
	}
	return result
}

func mergeVaryValue(header http.Header, value string) {
	if strings.TrimSpace(value) == "*" {
		header.Set("Vary", "*")
		return
	}
	if header.Get("Vary") == "*" {
		return
	}
	seen := make(map[string]bool)
	parts := make([]string, 0)
	for _, existing := range append(header.Values("Vary"), value) {
		for _, part := range strings.Split(existing, ",") {
			part = strings.TrimSpace(part)
			if part == "" || seen[strings.ToLower(part)] {
				continue
			}
			seen[strings.ToLower(part)] = true
			parts = append(parts, part)
		}
	}
	header.Del("Vary")
	if len(parts) > 0 {
		header.Set("Vary", strings.Join(parts, ", "))
	}
}

func clearHeader(header http.Header) {
	for name := range header {
		header.Del(name)
	}
}
