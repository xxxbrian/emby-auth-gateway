package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

const genericQueryAuthKey = "token"

var strictQueryAuthKeys = []string{"api_key", "access_token", "X-Emby-Token"}

var (
	errMalformedQuery     = errors.New("malformed query")
	errCredentialConflict = errors.New("credential conflict")
	errCredentialStore    = errors.New("credential store unavailable")
)

func isStrictQueryAuthKey(key string) bool {
	switch key {
	case "api_key", "access_token", "X-Emby-Token":
		return true
	default:
		return false
	}
}

// isEgressCredentialQueryKey is deliberately case-insensitive and broader
// than ingress selection. It is defense-in-depth at upstream boundaries only.
func isEgressCredentialQueryKey(key string) bool {
	switch strings.ToLower(key) {
	case "api_key", "access_token", "token", "x-emby-token", "x-mediabrowser-token":
		return true
	default:
		return false
	}
}

func isEgressCredentialAliasQueryKey(key string) bool {
	switch strings.ToLower(key) {
	case "api_key", "access_token", "x-emby-token", "x-mediabrowser-token":
		return true
	default:
		return false
	}
}

// IsGatewayShapedToken reports whether token matches the canonical gateway token
// encoding: 32 raw bytes as base64url without padding (43 characters), with
// decode+re-encode equality so non-canonical trailing bits are rejected.
func IsGatewayShapedToken(token string) bool {
	if len(token) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		return false
	}
	return base64.RawURLEncoding.EncodeToString(decoded) == token
}

// validateRawQuery reports whether rawQuery has valid URL encoding.
// Callers that accept query credentials must reject malformed queries with 400
// before any session/store lookup.
func validateRawQuery(rawQuery string) error {
	if _, err := parseRawQuery(rawQuery); err != nil {
		return errMalformedQuery
	}
	return nil
}

// acceptsClientCredentials reports routes that accept Emby client credentials
// (logout and the authenticated proxy catch-all). Anonymous public/login routes
// return false so their malformed-query behavior stays unchanged.
func acceptsClientCredentials(method, rel string) bool {
	switch {
	case method == http.MethodPost && equalPath(rel, "/Users/AuthenticateByName"):
		return false
	case method == http.MethodGet && equalPath(rel, "/System/Info/Public"):
		return false
	case (method == http.MethodGet || method == http.MethodPost) && equalPath(rel, "/System/Ping"):
		return false
	case method == http.MethodGet && equalPath(rel, "/Users/Public"):
		return false
	case method == http.MethodGet && equalPath(rel, "/Branding/Configuration"):
		return false
	case method == http.MethodGet && equalPath(rel, "/Branding/Css.css"):
		return false
	default:
		return true
	}
}

// guardProxyQueryCredentials applies the generic gateway-shaped token conflict
// guard. It assumes rawQuery was already validated and must run after the
// selected session is validated and before any HTTP/WebSocket upstream dial.
func (s *Server) guardProxyQueryCredentials(ctx context.Context, rawQuery, gatewayToken string) error {
	q, err := parseRawQuery(rawQuery)
	if err != nil {
		return errMalformedQuery
	}
	return s.guardGenericQueryTokens(ctx, q[genericQueryAuthKey], gatewayToken)
}

func (s *Server) guardGenericQueryTokens(ctx context.Context, values []string, gatewayToken string) error {
	shaped := make([]string, 0, 2)
	seen := map[string]struct{}{}
	for _, value := range values {
		token := strings.TrimSpace(value)
		if token == "" || token == gatewayToken {
			continue
		}
		if !IsGatewayShapedToken(token) {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		shaped = append(shaped, token)
		if len(shaped) > 1 {
			return errCredentialConflict
		}
	}
	if len(shaped) == 0 {
		return nil
	}

	exists, err := s.sessions.SessionTokenExists(ctx, HashToken(shaped[0]))
	if err != nil {
		return errCredentialStore
	}
	if exists {
		return errCredentialConflict
	}
	return nil
}

func rewriteProxyQueryValues(q url.Values, gatewayToken string, session *Session, upstream upstreamRequestSnapshot) {
	for key, vals := range q {
		if isEgressCredentialAliasQueryKey(key) {
			delete(q, key)
			continue
		}
		kept := vals[:0]
		for _, val := range vals {
			if gatewayToken != "" && val == gatewayToken {
				continue
			}
			if val == session.SyntheticUserID {
				val = upstream.userID
			}
			kept = append(kept, val)
		}
		if len(kept) == 0 {
			delete(q, key)
			continue
		}
		q[key] = kept
	}
	q.Set("api_key", upstream.token)
}

func writeCredentialQueryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errMalformedQuery), errors.Is(err, errCredentialConflict):
		http.Error(w, "bad request", http.StatusBadRequest)
	case errors.Is(err, errCredentialStore):
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	default:
		http.Error(w, "bad request", http.StatusBadRequest)
	}
}
