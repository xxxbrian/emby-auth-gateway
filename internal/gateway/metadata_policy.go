package gateway

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// ErrForbidden reports that a request attempted to select an identity it does
// not own. Callers should map this error without inspecting partial output.
var ErrForbidden = errors.New("forbidden")

var metadataListPolicies = map[string]struct {
	canonical string
	personal  map[string]struct{}
}{
	"filters": {
		canonical: "Filters",
		personal: foldedSet(
			"IsPlayed", "IsUnplayed", "IsFavorite", "IsResumable", "Likes", "Dislikes",
		),
	},
	"sortby": {
		canonical: "SortBy",
		personal: foldedSet(
			"DatePlayed", "PlayCount", "IsFavorite", "IsFavoriteOrLiked", "PlaybackPositionTicks", "PlayedPercentage",
		),
	},
	"fields": {
		canonical: "Fields",
		personal:  foldedSet("UserData"),
	},
}

var directPersonalMetadataKeys = foldedSet(
	"IsPlayed", "IsFavorite", "IsResumable", "IsLiked", "IsDisliked",
)

// SanitizeMetadataQuery returns deterministic query text for metadata egress.
// It never mutates input and binds all accepted UserId aliases to backendUserID.
func SanitizeMetadataQuery(input url.Values, syntheticUserID, backendUserID string) (string, error) {
	for key, values := range input {
		if strings.EqualFold(key, "UserId") && len(values) == 0 {
			return "", ErrForbidden
		}
	}
	return sanitizeMetadataRawQuery(input.Encode(), syntheticUserID, backendUserID, "")
}

func sanitizeMetadataRawQuery(rawQuery, syntheticUserID, backendUserID, gatewayToken string) (string, error) {
	if syntheticUserID == "" || backendUserID == "" {
		return "", ErrForbidden
	}
	pairs, err := parseRawQueryPairs(rawQuery)
	if err != nil {
		return "", fmt.Errorf("%w: malformed metadata query", ErrBadRequest)
	}
	out := make([]string, 0, len(pairs)+2)
	for _, pair := range pairs {
		foldedKey := strings.ToLower(pair.key)
		switch {
		case matchesSelectedGatewayCredential(pair.value, gatewayToken, ""):
			continue
		case isEgressCredentialQueryKey(foldedKey):
			continue
		case foldedKey == "userid":
			if !pair.hasEquals || pair.value != syntheticUserID {
				return "", ErrForbidden
			}
		case foldedKey == "enableuserdata" || foldedKey == "enableuserdatas":
			continue
		case containsFolded(directPersonalMetadataKeys, foldedKey):
			continue
		case metadataListPolicies[foldedKey].canonical != "":
			policy := metadataListPolicies[foldedKey]
			if sanitized, ok := sanitizeMetadataList(pair.value, policy.personal); ok {
				out = append(out, url.QueryEscape(policy.canonical)+"="+url.QueryEscape(sanitized))
			}
		default:
			out = append(out, pair.raw)
		}
	}
	out = append(out, "EnableUserData=false", "UserId="+url.QueryEscape(backendUserID))
	return strings.Join(out, "&"), nil
}

func sanitizeMetadataList(value string, personal map[string]struct{}) (string, bool) {
	members := strings.Split(value, ",")
	kept := members[:0]
	for _, member := range members {
		if _, remove := personal[strings.ToLower(strings.TrimSpace(member))]; !remove {
			kept = append(kept, member)
		}
	}
	if len(kept) == 0 {
		return "", false
	}
	return strings.Join(kept, ","), true
}

func sortedValueKeys(values url.Values) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := strings.ToLower(keys[i]), strings.ToLower(keys[j])
		if left == right {
			return keys[i] < keys[j]
		}
		return left < right
	})
	return keys
}

func foldedSet(values ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[strings.ToLower(value)] = struct{}{}
	}
	return set
}

func containsFolded(set map[string]struct{}, folded string) bool {
	_, ok := set[folded]
	return ok
}
