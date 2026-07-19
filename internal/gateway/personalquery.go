package gateway

import (
	"context"
	"net/url"
	"strings"
	"time"
)

type resolvedPersonalItem struct {
	item  map[string]any
	state PlaybackState
}

func isAllowedPersonalItemListPath(rel string) bool {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return false
	}
	if len(parts) == 3 && strings.EqualFold(parts[0], "Users") && strings.EqualFold(parts[2], "Items") {
		return true
	}
	if len(parts) == 1 && strings.EqualFold(parts[0], "Items") {
		return true
	}
	if len(parts) == 3 && strings.EqualFold(parts[0], "Shows") && (strings.EqualFold(parts[2], "Episodes") || strings.EqualFold(parts[2], "Seasons")) {
		return true
	}
	return false
}

func appendResolutionFields(q url.Values, sortBy []string) {
	fields := splitFilterValues(q["Fields"])
	seen := map[string]bool{}
	for _, field := range fields {
		seen[strings.ToLower(field)] = true
	}
	for _, sortName := range sortBy {
		for _, field := range resolutionFieldsForSort(sortName) {
			if !seen[strings.ToLower(field)] {
				fields = append(fields, field)
				seen[strings.ToLower(field)] = true
			}
		}
	}
	if len(fields) > 0 {
		q.Set("Fields", strings.Join(fields, ","))
	}
}

func resolutionFieldsForSort(sortName string) []string {
	switch strings.ToLower(sortName) {
	case "sortname":
		return []string{"SortName"}
	case "datecreated":
		return []string{"DateCreated"}
	case "premieredate":
		return []string{"PremiereDate"}
	case "communityrating":
		return []string{"CommunityRating"}
	case "criticrating":
		return []string{"CriticRating"}
	case "officialrating":
		return []string{"OfficialRating"}
	case "productionyear":
		return []string{"ProductionYear"}
	default:
		return nil
	}
}

func compareTime(a, b time.Time) int {
	if a.Equal(b) {
		return 0
	}
	if a.Before(b) {
		return -1
	}
	return 1
}

func compareInt(a, b int) int {
	if a == b {
		return 0
	}
	if a < b {
		return -1
	}
	return 1
}

func compareFloat(a, b float64) int {
	if a == b {
		return 0
	}
	if a < b {
		return -1
	}
	return 1
}

func totalRecordCount(value any) (int, bool) {
	obj, ok := value.(map[string]any)
	if !ok {
		return 0, false
	}
	if v, ok := int64Field(obj, "TotalRecordCount"); ok {
		return int(v), true
	}
	return 0, false
}

func learnChildCountsFromItems(ctx context.Context, store Store, session *Session, items []map[string]any) {
	if session == nil {
		return
	}
	byID := map[string]int{}
	order := make([]string, 0, len(items))
	for _, item := range items {
		itemID, _ := stringField(item, "Id")
		count := itemChildCount(item)
		if itemID == "" || count <= 0 {
			continue
		}
		if _, seen := byID[itemID]; !seen {
			order = append(order, itemID)
		}
		byID[itemID] = count
	}
	if len(byID) == 0 {
		return
	}
	counts := make([]ItemChildCount, 0, len(order))
	for _, itemID := range order {
		counts = append(counts, ItemChildCount{ItemID: itemID, ChildCount: byID[itemID]})
	}
	_ = store.SaveItemChildCounts(ctx, counts)
}
