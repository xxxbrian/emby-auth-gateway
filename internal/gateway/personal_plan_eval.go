package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

var ErrPersonalDuplicateState = errors.New("duplicate personal state item id")
var ErrPersonalEmptyStateID = errors.New("personal state has empty item id")
var ErrPersonalMalformedCandidate = errors.New("personal candidate has malformed item id")
var ErrPersonalDuplicateCandidate = errors.New("duplicate personal candidate item id")

func personalStateMatches(state PlaybackState, predicates personalPredicates) bool {
	return personalTruthMatches(predicates.Played, state.Played) &&
		personalTruthMatches(predicates.Favorite, state.IsFavorite) &&
		personalTruthMatches(predicates.Resumable, !state.Played && state.PlaybackPositionTicks > 0) &&
		personalRatingMatches(predicates.Rating, state.Likes)
}

func personalTruthMatches(want personalTruth, value bool) bool {
	return want == personalTruthAny || (want == personalTruthTrue && value) || (want == personalTruthFalse && !value)
}

func personalRatingMatches(want personalRating, likes *bool) bool {
	if want == personalRatingAny {
		return true
	}
	if likes == nil {
		return false
	}
	return (want == personalRatingLiked && *likes) || (want == personalRatingDisliked && !*likes)
}

func indexPersonalStates(states []PlaybackState) (map[string]PlaybackState, error) {
	indexed := make(map[string]PlaybackState, len(states))
	for _, state := range states {
		if state.ItemID == "" {
			return nil, fmt.Errorf("%w: state %q", ErrPersonalEmptyStateID, state.ID)
		}
		if _, exists := indexed[state.ItemID]; exists {
			return nil, fmt.Errorf("%w: %q", ErrPersonalDuplicateState, state.ItemID)
		}
		indexed[state.ItemID] = state
	}
	return indexed, nil
}

// joinPersonalCandidates excludes candidates without state when requireState is true.
// When false, absent state is represented by zero state with the candidate ID.
func joinPersonalCandidates(candidates []map[string]any, states map[string]PlaybackState, requireState bool) ([]resolvedPersonalItem, error) {
	joined := make([]resolvedPersonalItem, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, item := range candidates {
		id, ok := personalItemID(item)
		if !ok {
			return nil, fmt.Errorf("%w: %#v", ErrPersonalMalformedCandidate, item)
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("%w: %q", ErrPersonalDuplicateCandidate, id)
		}
		seen[id] = struct{}{}
		state, found := states[id]
		if !found {
			if requireState {
				continue
			}
			state.ItemID = id
		}
		joined = append(joined, resolvedPersonalItem{item: item, state: state})
	}
	return joined, nil
}

func personalItemID(item map[string]any) (string, bool) {
	for key, value := range item {
		if strings.EqualFold(key, "id") {
			id, ok := value.(string)
			return id, ok && id != ""
		}
	}
	return "", false
}

func sortPersonalPlanItems(items []resolvedPersonalItem, terms []personalSortTerm) {
	sort.SliceStable(items, func(i, j int) bool {
		for _, term := range terms {
			av, aok := personalSortValue(items[i], term)
			bv, bok := personalSortValue(items[j], term)
			if aok != bok {
				return aok
			}
			if !aok {
				continue
			}
			if cmp := comparePersonalSortValue(av, bv); cmp != 0 {
				if term.Direction == personalSortDescending {
					return cmp > 0
				}
				return cmp < 0
			}
		}
		return personalItemIDForSort(items[i]) < personalItemIDForSort(items[j])
	})
}

type personalComparable struct {
	kind   byte
	number *big.Rat
	date   time.Time
	text   string
	raw    string
}

func personalSortValue(item resolvedPersonalItem, term personalSortTerm) (personalComparable, bool) {
	name := strings.ToLower(term.Name)
	var value any
	var ok bool
	if term.Source == personalSortLocal {
		value, ok = personalLocalSortValue(item.state, name)
	} else {
		for key, candidate := range item.item {
			if strings.ToLower(key) == name {
				value, ok = candidate, candidate != nil
				break
			}
		}
	}
	if !ok {
		return personalComparable{}, false
	}
	return normalizePersonalSortValue(value)
}

func personalLocalSortValue(state PlaybackState, name string) (any, bool) {
	switch name {
	case "dateplayed", "lastplayeddate":
		if state.LastPlayedDate != nil {
			return *state.LastPlayedDate, true
		}
	case "updatedat":
		return state.UpdatedAt, !state.UpdatedAt.IsZero()
	case "playcount":
		return state.PlayCount, true
	case "isfavorite":
		return state.IsFavorite, true
	case "isfavoriteorliked":
		return state.IsFavorite || (state.Likes != nil && *state.Likes), true
	case "playbackpositionticks":
		return state.PlaybackPositionTicks, true
	case "playedpercentage":
		if state.PlayedPercentage != nil {
			return *state.PlayedPercentage, true
		}
	case "seriesactivity":
		if state.LastPlayedDate != nil {
			return *state.LastPlayedDate, true
		}
		return state.UpdatedAt, !state.UpdatedAt.IsZero()
	}
	return nil, false
}

func normalizePersonalSortValue(value any) (personalComparable, bool) {
	switch v := value.(type) {
	case bool:
		return personalComparable{kind: 'b', number: boolNumber(v)}, true
	case time.Time:
		return personalComparable{kind: 'd', date: v}, !v.IsZero()
	case string:
		if d, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return personalComparable{kind: 'd', date: d, raw: v}, true
		}
		return personalComparable{kind: 's', text: strings.ToLower(v), raw: v}, true
	case json.Number, int, int64, float64, float32:
		number, raw, ok := exactPersonalComparableNumber(v)
		return personalComparable{kind: 'n', number: number, raw: raw}, ok
	}
	return personalComparable{}, false
}

func boolNumber(v bool) *big.Rat {
	if v {
		return big.NewRat(1, 1)
	}
	return big.NewRat(0, 1)
}

func comparePersonalSortValue(a, b personalComparable) int {
	if a.kind == b.kind {
		switch a.kind {
		case 'b', 'n':
			return a.number.Cmp(b.number)
		case 'd':
			return compareTime(a.date, b.date)
		case 's':
			if a.text < b.text {
				return -1
			}
			if a.text > b.text {
				return 1
			}
			if a.raw < b.raw {
				return -1
			}
			if a.raw > b.raw {
				return 1
			}
			return 0
		}
	}
	if a.kind < b.kind {
		return -1
	}
	return 1
}

func personalItemIDForSort(item resolvedPersonalItem) string {
	if item.state.ItemID != "" {
		return item.state.ItemID
	}
	id, _ := personalItemID(item.item)
	return id
}

func pagePersonalPlanItems(items []resolvedPersonalItem, spec personalPageSpec) []resolvedPersonalItem {
	start := spec.Start
	if start < 0 {
		start = 0
	}
	if start >= len(items) {
		return []resolvedPersonalItem{}
	}
	if spec.Limit != nil {
		if *spec.Limit <= 0 {
			return []resolvedPersonalItem{}
		}
		end := start + *spec.Limit
		if end > len(items) {
			end = len(items)
		}
		return append([]resolvedPersonalItem(nil), items[start:end]...)
	}
	return append([]resolvedPersonalItem(nil), items[start:]...)
}
