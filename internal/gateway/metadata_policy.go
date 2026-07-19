package gateway

import (
	"errors"
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
	if syntheticUserID == "" || backendUserID == "" {
		return "", ErrForbidden
	}

	keys := sortedValueKeys(input)
	out := make(url.Values, len(input)+2)
	lists := make(map[string][]string, len(metadataListPolicies))

	for _, key := range keys {
		values := input[key]
		foldedKey := strings.ToLower(key)
		switch {
		case isEgressCredentialQueryKey(foldedKey):
			// Managed metadata authentication is header-only.
		case foldedKey == "userid":
			if len(values) == 0 {
				return "", ErrForbidden
			}
			for _, value := range values {
				if value != syntheticUserID {
					return "", ErrForbidden
				}
			}
		case foldedKey == "enableuserdata" || foldedKey == "enableuserdatas":
			// The canonical false value is emitted after all aliases are removed.
		case containsFolded(directPersonalMetadataKeys, foldedKey):
			// Direct personal predicates must be evaluated by the gateway.
		case metadataListPolicies[foldedKey].canonical != "":
			policy := metadataListPolicies[foldedKey]
			for _, value := range values {
				if sanitized, ok := sanitizeMetadataList(value, policy.personal); ok {
					lists[foldedKey] = append(lists[foldedKey], sanitized)
				}
			}
		default:
			out[key] = append([]string(nil), values...)
		}
	}

	for foldedKey, values := range lists {
		if len(values) != 0 {
			out[metadataListPolicies[foldedKey].canonical] = values
		}
	}
	out.Set("EnableUserData", "false")
	out.Set("UserId", backendUserID)
	return out.Encode(), nil
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
