package gateway

import (
	"fmt"
	"net/url"
	"strings"
)

type rawQueryPair struct {
	raw       string
	rawKey    string
	key       string
	value     string
	hasEquals bool
}

func projectUserPath(rawPath, syntheticUserID, backendUserID string) string {
	parts := strings.Split(rawPath, "/")
	start := 0
	if len(parts) > 0 && parts[0] == "" {
		start = 1
	}
	if len(parts)-start >= 2 && strings.EqualFold(parts[start], "Users") && parts[start+1] == syntheticUserID {
		parts[start+1] = backendUserID
	}
	return strings.Join(parts, "/")
}

func parseRawQueryPairs(rawQuery string) ([]rawQueryPair, error) {
	if rawQuery == "" {
		return nil, nil
	}
	segments := strings.Split(rawQuery, "&")
	pairs := make([]rawQueryPair, 0, len(segments))
	for _, segment := range segments {
		rawKey, rawValue, hasEquals := strings.Cut(segment, "=")
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			return nil, err
		}
		value := ""
		if hasEquals {
			value, err = url.QueryUnescape(rawValue)
			if err != nil {
				return nil, err
			}
		}
		pairs = append(pairs, rawQueryPair{raw: segment, rawKey: rawKey, key: key, value: value, hasEquals: hasEquals})
	}
	return pairs, nil
}

func replaceRawQueryValue(pair rawQueryPair, value string) string {
	return pair.rawKey + "=" + url.QueryEscape(value)
}

func rewriteProxyRawQuery(rawQuery string, session *Session, upstream upstreamRequestSnapshot) (string, error) {
	if session == nil {
		return "", fmt.Errorf("%w: missing session", ErrBadRequest)
	}
	pairs, err := parseRawQueryPairs(rawQuery)
	if err != nil {
		return "", err
	}
	out := make([]string, 0, len(pairs)+1)
	for _, pair := range pairs {
		switch {
		case matchesSelectedGatewayCredential(pair.value, "", session.GatewayTokenHash):
			continue
		case isEgressCredentialQueryKey(pair.key):
			continue
		case strings.EqualFold(pair.key, "UserId"):
			if !pair.hasEquals || pair.value != session.SyntheticUserID {
				return "", ErrForbidden
			}
			out = append(out, replaceRawQueryValue(pair, upstream.userID))
		default:
			out = append(out, pair.raw)
		}
	}
	if upstream.token != "" {
		out = append(out, "api_key="+url.QueryEscape(upstream.token))
	}
	return strings.Join(out, "&"), nil
}
