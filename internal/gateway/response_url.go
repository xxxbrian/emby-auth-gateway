package gateway

import (
	"net/url"
	"path"
	"regexp"
	"strings"
)

var m3u8URIAttribute = regexp.MustCompile(`URI="([^"]*)"`)

// rewriteMediaReference returns an API-relative owned media reference. A
// specialized reference that contains the backend token is never returned.
func rewriteMediaReference(value string, session *Session, gatewayToken, publicGatewayBase, gatewayServerID string, absolute bool) string {
	if hasURLControls(value) {
		return ""
	}
	u, err := url.Parse(value)
	if err != nil {
		return ""
	}
	if u.IsAbs() {
		if u.Host == "" || u.User != nil || (!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) {
			return safeOutput(value, session.BackendToken)
		}
		if media, ok := qualifyingMediaPath(u, session, publicGatewayBase); ok {
			u.RawQuery = rewriteOwnedQuery(u.RawQuery, session, gatewayToken, gatewayServerID)
			return safeOutput(formatOwnedURL(media, u, publicGatewayBase, absolute), session.BackendToken)
		}
		return safeOutput(value, session.BackendToken)
	}
	if u.Host != "" { // Network-path references are external, never local paths.
		return safeOutput(value, session.BackendToken)
	}
	if media, ok := relativeMediaPath(u, publicGatewayBase); ok {
		u.RawQuery = rewriteOwnedQuery(u.RawQuery, session, gatewayToken, gatewayServerID)
		return safeOutput(formatOwnedURL(media, u, publicGatewayBase, absolute), session.BackendToken)
	}
	return safeOutput(value, session.BackendToken)
}

func unsafeReference(value, backendToken string) bool {
	return hasURLControls(value) || hasMalformedPercentEscape(value) || containsBackendToken(value, backendToken)
}

// hasMalformedPercentEscape validates the raw reference before URL/component
// decoding. Valid percent-encoded UTF-8 and reserved delimiters are accepted.
func hasMalformedPercentEscape(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] != '%' {
			continue
		}
		if i+2 >= len(value) || !isHex(value[i+1]) || !isHex(value[i+2]) {
			return true
		}
		i += 2
	}
	return false
}

func isHex(b byte) bool {
	return ('0' <= b && b <= '9') || ('a' <= b && b <= 'f') || ('A' <= b && b <= 'F')
}

func containsBackendToken(value, backendToken string) bool {
	if backendToken == "" {
		return false
	}
	if strings.Contains(value, backendToken) || strings.Contains(decodedURLComponent(value), backendToken) {
		return true
	}
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	for _, component := range []string{u.EscapedPath(), u.RawQuery, u.EscapedFragment(), u.User.String()} {
		if strings.Contains(component, backendToken) || strings.Contains(decodedURLComponent(component), backendToken) {
			return true
		}
	}
	return false
}

func decodedURLComponent(value string) string {
	if decoded, err := url.QueryUnescape(value); err == nil {
		return decoded
	}
	if decoded, err := url.PathUnescape(value); err == nil {
		return decoded
	}
	return value
}

func safeOutput(value, backendToken string) string {
	if unsafeReference(value, backendToken) {
		return ""
	}
	return value
}

func formatOwnedURL(mediaPath string, u *url.URL, publicGatewayBase string, absolute bool) string {
	if absolute {
		return strings.TrimRight(publicGatewayBase, "/") + mediaPath + queryAndFragment(u)
	}
	return mediaPath + queryAndFragment(u)
}

func qualifyingMediaPath(u *url.URL, session *Session, publicGatewayBase string) (string, bool) {
	pathValue := u.EscapedPath()
	if suffix, ok := configuredOriginSuffix(u, session); ok {
		if media, ok := mediaPathAfterPrefix(suffix, ""); ok {
			return media, true
		}
	}
	public, publicErr := url.Parse(publicGatewayBase)
	if publicErr == nil && sameOrigin(u, public) {
		if media, ok := mediaPathAfterPrefix(pathValue, public.EscapedPath()); ok {
			return media, true
		}
		return conventionalMediaPath(pathValue)
	}
	return conventionalMediaPath(pathValue)
}

// configuredOriginSuffix recognizes backend origin path variants and returns a
// gateway-relative suffix. Exact configured non-root bases win; conventional
// prefixes then cover backend deployments that publish an alternate base.
func configuredOriginSuffix(u *url.URL, session *Session) (string, bool) {
	backend, err := url.Parse(session.BackendBaseURL)
	if err != nil || !sameOrigin(u, backend) {
		return "", false
	}
	configured := strings.TrimRight(backend.EscapedPath(), "/")
	for _, conventional := range []string{"/emby", "/mediabrowser"} {
		foldedPath, foldedPrefix := strings.ToLower(u.EscapedPath()), strings.ToLower(conventional)
		if strings.HasPrefix(foldedPath, foldedPrefix) && foldedPath != foldedPrefix && !strings.HasPrefix(foldedPath, foldedPrefix+"/") {
			return "", false
		}
	}
	prefixes := []string{configured, "/emby", "/mediabrowser", ""}
	if configured == "" {
		// A root backend has no discriminating exact base. Prefer conventional
		// bases so returned /emby or /mediabrowser paths do not duplicate ours.
		prefixes = []string{"/emby", "/mediabrowser", ""}
	}
	seen := map[string]struct{}{}
	for _, prefix := range prefixes {
		key := strings.ToLower(prefix)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		if suffix, ok := pathSuffixAfterPrefix(u.EscapedPath(), prefix); ok {
			return suffix, true
		}
	}
	return "", false
}

func pathSuffixAfterPrefix(escapedPath, prefix string) (string, bool) {
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		return escapedPath, strings.HasPrefix(escapedPath, "/")
	}
	if strings.EqualFold(escapedPath, prefix) {
		return "/", true
	}
	if !strings.HasPrefix(strings.ToLower(escapedPath), strings.ToLower(prefix)+"/") {
		return "", false
	}
	return escapedPath[len(prefix):], true
}

func conventionalMediaPath(escapedPath string) (string, bool) {
	for _, prefix := range []string{"", "/emby", "/mediabrowser"} {
		if media, ok := mediaPathAfterPrefix(escapedPath, prefix); ok {
			return media, true
		}
	}
	return "", false
}

func relativeMediaPath(u *url.URL, publicGatewayBase string) (string, bool) {
	if public, err := url.Parse(publicGatewayBase); err == nil {
		if media, ok := mediaPathAfterPrefix(u.EscapedPath(), public.EscapedPath()); ok {
			return media, true
		}
	}
	return conventionalMediaPath(u.EscapedPath())
}

func mediaPathAfterPrefix(escapedPath, prefix string) (string, bool) {
	prefix = strings.TrimRight(prefix, "/")
	if prefix != "" {
		if !strings.HasPrefix(strings.ToLower(escapedPath), strings.ToLower(prefix)+"/") {
			return "", false
		}
		escapedPath = escapedPath[len(prefix):]
	}
	parts := strings.Split(strings.TrimPrefix(escapedPath, "/"), "/")
	if len(parts) < 3 || (!strings.EqualFold(parts[0], "Videos") && !strings.EqualFold(parts[0], "Audio")) || parts[1] == "" {
		return "", false
	}
	return "/" + strings.Join(parts, "/"), true
}

func rewriteOwnedQuery(raw string, session *Session, gatewayToken, gatewayServerID string) string {
	parts := strings.Split(raw, "&")
	for i, part := range parts {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		decoded, err := url.QueryUnescape(value)
		if err != nil {
			continue
		}
		switch {
		case session.BackendToken != "" && decoded == session.BackendToken:
			parts[i] = key + "=" + url.QueryEscape(gatewayToken)
		case session.BackendUserID != "" && decoded == session.BackendUserID:
			parts[i] = key + "=" + url.QueryEscape(session.SyntheticUserID)
		case session.BackendServerID != "" && decoded == session.BackendServerID:
			parts[i] = key + "=" + url.QueryEscape(gatewayServerID)
		}
	}
	return strings.Join(parts, "&")
}

func queryAndFragment(u *url.URL) string {
	result := ""
	if u.ForceQuery || u.RawQuery != "" {
		result += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		result += "#" + u.EscapedFragment()
	}
	return result
}

func rewriteM3U8MediaReferences(data []byte, playlistRel string, session *Session, gatewayToken, publicGatewayBase, gatewayServerID string) []byte {
	newline := "\n"
	if strings.Contains(string(data), "\r\n") {
		newline = "\r\n"
	}
	lines := strings.Split(string(data), newline)
	for i, line := range lines {
		if !strings.HasPrefix(line, "#") {
			lines[i] = rewriteM3U8Reference(line, playlistRel, session, gatewayToken, publicGatewayBase, gatewayServerID)
			continue
		}
		lines[i] = m3u8URIAttribute.ReplaceAllStringFunc(line, func(match string) string {
			value := strings.TrimSuffix(strings.TrimPrefix(match, `URI="`), `"`)
			return `URI="` + rewriteM3U8Reference(value, playlistRel, session, gatewayToken, publicGatewayBase, gatewayServerID) + `"`
		})
	}
	return []byte(strings.Join(lines, newline))
}

func rewriteM3U8Reference(value, playlistRel string, session *Session, gatewayToken, publicGatewayBase, gatewayServerID string) string {
	if hasURLControls(value) {
		return ""
	}
	u, err := url.Parse(value)
	if err != nil {
		return ""
	}
	if u.Host != "" && !u.IsAbs() {
		return safeOutput(value, session.BackendToken)
	}
	if u.IsAbs() {
		if _, media := qualifyingMediaPath(u, session, publicGatewayBase); media {
			return rewriteMediaReference(value, session, gatewayToken, publicGatewayBase, gatewayServerID, true)
		}
		if pathValue, ok := configuredOriginSuffix(u, session); ok {
			u.RawQuery = rewriteOwnedQuery(u.RawQuery, session, gatewayToken, gatewayServerID)
			return safeOutput(strings.TrimRight(publicGatewayBase, "/")+pathValue+queryAndFragment(u), session.BackendToken)
		}
		return safeOutput(value, session.BackendToken)
	}
	resolved := resolveRelativePath(u.EscapedPath(), playlistRel, publicGatewayBase)
	if resolved == "" {
		return safeOutput(value, session.BackendToken)
	}
	u.RawQuery = rewriteOwnedQuery(u.RawQuery, session, gatewayToken, gatewayServerID)
	return safeOutput(strings.TrimRight(publicGatewayBase, "/")+resolved+queryAndFragment(u), session.BackendToken)
}

func resolveRelativePath(reference, requestRel, publicGatewayBase string) string {
	if reference == "" {
		return ""
	}
	public, err := url.Parse(publicGatewayBase)
	if err != nil {
		return ""
	}
	base := strings.TrimRight(public.EscapedPath(), "/")
	if strings.HasPrefix(reference, "/") {
		if base != "" && strings.HasPrefix(strings.ToLower(reference), strings.ToLower(base)+"/") {
			return reference[len(base):]
		}
		return reference
	}
	return path.Join(path.Dir(requestRel), reference)
}

func rewriteResponseLocation(value, requestRel string, session *Session, gatewayToken, publicGatewayBase, gatewayServerID string) string {
	if hasURLControls(value) {
		return ""
	}
	u, err := url.Parse(value)
	if err != nil {
		return ""
	}
	if u.Host != "" && !u.IsAbs() {
		return safeOutput(value, session.BackendToken)
	}
	if !u.IsAbs() {
		resolved := resolveRelativePath(u.EscapedPath(), requestRel, publicGatewayBase)
		if u.EscapedPath() == "" { // query-only retains the request URI.
			resolved = requestRel
		}
		if resolved == "" {
			return ""
		}
		u.RawQuery = rewriteOwnedQuery(u.RawQuery, session, gatewayToken, gatewayServerID)
		return safeOutput(strings.TrimRight(publicGatewayBase, "/")+resolved+queryAndFragment(u), session.BackendToken)
	}
	if _, media := qualifyingMediaPath(u, session, publicGatewayBase); media {
		return rewriteMediaReference(value, session, gatewayToken, publicGatewayBase, gatewayServerID, true)
	}
	if pathValue, ok := configuredOriginSuffix(u, session); ok {
		u.RawQuery = rewriteOwnedQuery(u.RawQuery, session, gatewayToken, gatewayServerID)
		return safeOutput(strings.TrimRight(publicGatewayBase, "/")+pathValue+queryAndFragment(u), session.BackendToken)
	}
	return safeOutput(value, session.BackendToken)
}

func hasURLControls(value string) bool { return strings.ContainsAny(value, "\r\n") }
