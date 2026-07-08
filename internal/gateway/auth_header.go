package gateway

import (
	"net/http"
	"sort"
	"strings"
)

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

func ExtractToken(r *http.Request) string {
	for _, name := range []string{"X-Emby-Token", "X-MediaBrowser-Token"} {
		if token := strings.TrimSpace(r.Header.Get(name)); token != "" {
			return token
		}
	}
	for _, name := range []string{"X-Emby-Authorization", "Authorization"} {
		if auth := ParseEmbyAuthHeader(r.Header.Get(name)); auth.Token != "" {
			return auth.Token
		}
	}
	q := r.URL.Query()
	for _, name := range []string{"api_key", "access_token", "token"} {
		if token := strings.TrimSpace(q.Get(name)); token != "" {
			return token
		}
	}
	return ""
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
