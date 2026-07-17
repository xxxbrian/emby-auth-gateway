package gateway

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// TokenSource identifies where ExtractCredential found the selected token.
type TokenSource int

const (
	TokenSourceNone TokenSource = iota
	TokenSourceTokenHeader
	TokenSourceAuthHeader
	TokenSourceStrictQuery
	TokenSourceGenericQuery
)

// ExtractedCredential is the selected client credential and its source metadata.
type ExtractedCredential struct {
	Token    string
	Source   TokenSource
	QueryKey string
}

// ClientIdentity is the Emby client identity extracted for login sessions.
type ClientIdentity struct {
	Client   string
	Device   string
	DeviceID string
	Version  string
}

// ExtractClientIdentity selects client identity fields with Emby-compatible
// per-field precedence:
//  1. Selected structured authorization header fields
//  2. Matching standalone X-Emby-* header
//  3. Matching exact-case query parameter
//
// Structured authorization selection uses the first trimmed non-empty
// X-Emby-Authorization value, else the first trimmed non-empty Authorization
// value. Once selected, the other header and later repeated values are not
// inspected for structured fields; missing fields still fall back to
// standalone headers and query parameters.
func ExtractClientIdentity(r *http.Request) ClientIdentity {
	var identity ClientIdentity
	var auth AuthHeader
	hasAuth := false
	if first := firstNonEmptyHeaderValue(r.Header, "X-Emby-Authorization"); first != "" {
		auth = ParseEmbyAuthHeader(first)
		hasAuth = true
	} else if first := firstNonEmptyHeaderValue(r.Header, "Authorization"); first != "" {
		auth = ParseEmbyAuthHeader(first)
		hasAuth = true
	}

	q := r.URL.Query()

	if hasAuth {
		if v := strings.TrimSpace(auth.Client); v != "" {
			identity.Client = v
		}
	}
	if identity.Client == "" {
		if v := strings.TrimSpace(firstNonEmptyHeaderValue(r.Header, "X-Emby-Client")); v != "" {
			identity.Client = v
		}
	}
	if identity.Client == "" {
		identity.Client = firstNonEmptyQueryValue(q["X-Emby-Client"])
	}

	if hasAuth {
		if v := decodeDeviceName(auth.Device); v != "" {
			identity.Device = v
		}
	}
	if identity.Device == "" {
		if v := decodeDeviceName(firstNonEmptyHeaderValue(r.Header, "X-Emby-Device-Name")); v != "" {
			identity.Device = v
		}
	}
	if identity.Device == "" {
		identity.Device = firstNonEmptyQueryValue(q["X-Emby-Device-Name"])
	}

	if hasAuth {
		if v := strings.TrimSpace(auth.DeviceID); v != "" {
			identity.DeviceID = v
		}
	}
	if identity.DeviceID == "" {
		if v := strings.TrimSpace(firstNonEmptyHeaderValue(r.Header, "X-Emby-Device-Id")); v != "" {
			identity.DeviceID = v
		}
	}
	if identity.DeviceID == "" {
		identity.DeviceID = firstNonEmptyQueryValue(q["X-Emby-Device-Id"])
	}

	if hasAuth {
		if v := strings.TrimSpace(auth.Version); v != "" {
			identity.Version = v
		}
	}
	if identity.Version == "" {
		if v := strings.TrimSpace(firstNonEmptyHeaderValue(r.Header, "X-Emby-Client-Version")); v != "" {
			identity.Version = v
		}
	}
	if identity.Version == "" {
		identity.Version = firstNonEmptyQueryValue(q["X-Emby-Client-Version"])
	}

	return identity
}

func decodeDeviceName(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if decoded, err := url.QueryUnescape(raw); err == nil {
		return strings.TrimSpace(decoded)
	}
	return raw
}

func ParseEmbyAuthHeader(value string) AuthHeader {
	value = strings.TrimSpace(value)
	if value == "" {
		return AuthHeader{Fields: map[string]string{}}
	}

	parts := strings.SplitN(value, " ", 2)
	header := AuthHeader{Scheme: parts[0], Fields: map[string]string{}}
	if len(parts) == 1 {
		return header
	}

	for _, part := range splitHeaderFields(parts[1]) {
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), "\"")
		header.Fields[key] = val
		switch strings.ToLower(key) {
		case "userid":
			header.UserID = val
		case "client":
			header.Client = val
		case "device":
			header.Device = val
		case "deviceid":
			header.DeviceID = val
		case "version":
			header.Version = val
		case "token":
			header.Token = val
		}
	}

	return header
}

func (h AuthHeader) String() string {
	scheme := h.Scheme
	if scheme == "" {
		scheme = "Emby"
	}
	fields := map[string]string{}
	for k, v := range h.Fields {
		fields[k] = v
	}
	if h.UserID != "" {
		fields["UserId"] = h.UserID
	}
	if h.Client != "" {
		fields["Client"] = h.Client
	}
	if h.Device != "" {
		fields["Device"] = h.Device
	}
	if h.DeviceID != "" {
		fields["DeviceId"] = h.DeviceID
	}
	if h.Version != "" {
		fields["Version"] = h.Version
	}
	if h.Token != "" {
		fields["Token"] = h.Token
	}

	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"=\""+strings.ReplaceAll(fields[k], "\"", "")+"\"")
	}
	return scheme + " " + strings.Join(parts, ", ")
}

// ExtractToken returns the selected credential token using Emby-compatible precedence.
// It is a compatibility wrapper around ExtractCredential.
func ExtractToken(r *http.Request) string {
	return ExtractCredential(r).Token
}

// ExtractCredential selects one client credential by precedence:
//  1. X-Emby-Token / X-MediaBrowser-Token headers
//  2. X-Emby-Authorization / Authorization Emby token fields
//  3. strict query keys api_key, access_token, X-Emby-Token (case-sensitive)
//  4. generic query key token
//
// A non-empty higher-priority value wins and never falls back. Query keys are
// case-sensitive; within a key the first trimmed non-empty value is used.
// Repeated token headers use the first trimmed non-empty value per header name.
// Repeated Authorization/X-Emby-Authorization values also use the first trimmed
// non-empty field-value per header key: if that value is not a token-bearing
// Emby/MediaBrowser authorization, later values for the same key are not
// scanned and lower-priority query credentials are not considered.
func ExtractCredential(r *http.Request) ExtractedCredential {
	for _, name := range []string{"X-Emby-Token", "X-MediaBrowser-Token"} {
		if token := firstNonEmptyHeaderValue(r.Header, name); token != "" {
			return ExtractedCredential{Token: token, Source: TokenSourceTokenHeader}
		}
	}
	for _, name := range []string{"X-Emby-Authorization", "Authorization"} {
		first := firstNonEmptyHeaderValue(r.Header, name)
		if first == "" {
			continue
		}
		// First nonempty value is authoritative for this header key.
		if token, ok := embyAuthHeaderToken(first); ok {
			return ExtractedCredential{Token: token, Source: TokenSourceAuthHeader}
		}
		// Non-token-bearing / non-Emby auth: do not scan later values or fall back.
		return ExtractedCredential{}
	}

	q := r.URL.Query()
	for _, name := range strictQueryAuthKeys {
		if token := firstNonEmptyQueryValue(q[name]); token != "" {
			return ExtractedCredential{Token: token, Source: TokenSourceStrictQuery, QueryKey: name}
		}
	}
	if token := firstNonEmptyQueryValue(q[genericQueryAuthKey]); token != "" {
		return ExtractedCredential{Token: token, Source: TokenSourceGenericQuery, QueryKey: genericQueryAuthKey}
	}
	return ExtractedCredential{}
}

// embyAuthHeaderToken reports whether value is a token-bearing Emby/MediaBrowser
// authorization header field-value and returns its token.
func embyAuthHeaderToken(value string) (string, bool) {
	auth := ParseEmbyAuthHeader(value)
	token := strings.TrimSpace(auth.Token)
	if token == "" {
		return "", false
	}
	if !strings.EqualFold(auth.Scheme, "Emby") && !strings.EqualFold(auth.Scheme, "MediaBrowser") {
		return "", false
	}
	return token, true
}

func firstNonEmptyHeaderValue(h http.Header, name string) string {
	for _, value := range h.Values(name) {
		if token := strings.TrimSpace(value); token != "" {
			return token
		}
	}
	return ""
}

func firstNonEmptyQueryValue(values []string) string {
	for _, value := range values {
		if token := strings.TrimSpace(value); token != "" {
			return token
		}
	}
	return ""
}

func parseRawQuery(rawQuery string) (url.Values, error) {
	return url.ParseQuery(rawQuery)
}

func splitHeaderFields(s string) []string {
	var fields []string
	var current strings.Builder
	inQuote := false
	for _, r := range s {
		switch r {
		case '"':
			inQuote = !inQuote
			current.WriteRune(r)
		case ',':
			if inQuote {
				current.WriteRune(r)
				continue
			}
			if part := strings.TrimSpace(current.String()); part != "" {
				fields = append(fields, part)
			}
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	if part := strings.TrimSpace(current.String()); part != "" {
		fields = append(fields, part)
	}
	return fields
}
