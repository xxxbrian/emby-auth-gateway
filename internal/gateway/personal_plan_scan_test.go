package gateway

import (
	"errors"
	"strings"
	"testing"
)

func scanPolicy(pageSize, maxPages, maxItems int) personalScanPolicy {
	return personalScanPolicy{PageSize: pageSize, MaxPages: maxPages, MaxItems: maxItems}
}

func scanItems(ids ...string) []map[string]any {
	items := make([]map[string]any, len(ids))
	for i, id := range ids {
		items[i] = map[string]any{"Id": id}
	}
	return items
}

func scanTotal(value int) *int { return &value }

func TestPersonalPlanScan(t *testing.T) {
	tests := []struct {
		name       string
		policy     personalScanPolicy
		pages      []personalCandidatePage
		wantErr    string
		incomplete bool
		complete   bool
		count      int
	}{
		{"stable total", scanPolicy(2, 10, 10), []personalCandidatePage{
			{RequestedStart: 0, Items: scanItems("a", "b"), Total: scanTotal(3)},
			{RequestedStart: 2, Items: scanItems("c"), Total: scanTotal(3)},
		}, "", false, true, 3},
		{"no-total short terminal", scanPolicy(3, 10, 10), []personalCandidatePage{{RequestedStart: 0, Items: scanItems("a")}}, "", false, true, 1},
		{"exact page bound completion", scanPolicy(2, 1, 2), []personalCandidatePage{{RequestedStart: 0, Items: scanItems("a", "b"), Total: scanTotal(2)}}, "", false, true, 2},
		{"page bound incomplete", scanPolicy(2, 1, 10), []personalCandidatePage{{RequestedStart: 0, Items: scanItems("a", "b")}}, "", true, false, 2},
		{"item bound incomplete", scanPolicy(2, 10, 2), []personalCandidatePage{{RequestedStart: 0, Items: scanItems("a", "b")}}, "", true, false, 2},
		{"item bound crossing incomplete", scanPolicy(2, 10, 3), []personalCandidatePage{{RequestedStart: 0, Items: scanItems("a", "b")}, {RequestedStart: 2, Items: scanItems("c", "d")}}, "", true, false, 2},
		{"changing total", scanPolicy(2, 10, 10), []personalCandidatePage{{RequestedStart: 0, Items: scanItems("a", "b"), Total: scanTotal(3)}, {RequestedStart: 2, Items: scanItems("c", "d"), Total: scanTotal(4)}}, "total changed", false, false, 2},
		{"negative total", scanPolicy(2, 10, 10), []personalCandidatePage{{RequestedStart: 0, Items: nil, Total: scanTotal(-1)}}, "negative", false, false, 0},
		{"undersized total", scanPolicy(2, 10, 10), []personalCandidatePage{{RequestedStart: 0, Items: scanItems("a", "b"), Total: scanTotal(1)}}, "consumed", false, false, 0},
		{"empty before total", scanPolicy(2, 10, 10), []personalCandidatePage{{RequestedStart: 0, Items: nil, Total: scanTotal(1)}}, "before total", false, false, 0},
		{"duplicate IDs", scanPolicy(2, 10, 10), []personalCandidatePage{{RequestedStart: 0, Items: scanItems("a", "b")}, {RequestedStart: 2, Items: scanItems("a")}}, "duplicate", false, false, 2},
		{"missing IDs", scanPolicy(2, 10, 10), []personalCandidatePage{{RequestedStart: 0, Items: []map[string]any{{"Name": "missing"}}}}, "missing", false, false, 0},
		{"returned/requested mismatch", scanPolicy(2, 10, 10), []personalCandidatePage{{RequestedStart: 0, ReturnedStart: intPtr(1), Items: scanItems("a")}}, "returned start", false, false, 0},
		{"requested start mismatch", scanPolicy(2, 10, 10), []personalCandidatePage{{RequestedStart: 1, Items: scanItems("a")}}, "expected 0", false, false, 0},
		{"oversize page", scanPolicy(2, 10, 10), []personalCandidatePage{{RequestedStart: 0, Items: scanItems("a", "b", "c")}}, "maximum", false, false, 0},
		{"claimed terminal full page", scanPolicy(2, 10, 10), []personalCandidatePage{{RequestedStart: 0, Items: scanItems("a", "b"), Terminal: true}}, "terminal full", false, false, 0},
		{"progression", scanPolicy(2, 10, 10), []personalCandidatePage{{RequestedStart: 0, Items: scanItems("a", "b")}, {RequestedStart: 2, Items: scanItems("c")}}, "", false, true, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scan := newPersonalPlanScan(tt.policy)
			var err error
			for _, page := range tt.pages {
				err = scan.Add(page)
				if err != nil {
					break
				}
			}
			if tt.incomplete {
				if !errors.Is(err, ErrPersonalScanIncomplete) {
					t.Fatalf("error = %v, want incomplete", err)
				}
			} else if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Add() error = %v", err)
			}
			if scan.Complete() != tt.complete || scan.CandidateCount() != tt.count {
				t.Fatalf("state = complete %v, count %d", scan.Complete(), scan.CandidateCount())
			}
		})
	}
}

func TestPersonalPlanScanRejectsUnsafeItemIDs(t *testing.T) {
	for _, id := range []string{
		"bad/id",
		"bad\x00id",
		strings.Repeat("x", sessionCommandMaxItemIDBytes+1),
	} {
		scan := newPersonalPlanScan(scanPolicy(2, 10, 10))
		err := scan.Add(personalCandidatePage{RequestedStart: 0, Items: scanItems(id)})
		if err == nil || errors.Is(err, ErrPersonalScanIncomplete) || !strings.Contains(err.Error(), "unsafe Id") {
			t.Fatalf("Id %q error = %v", id, err)
		}
	}
}

func TestPersonalPlanScanAcceptsStandardItemIDs(t *testing.T) {
	for _, id := range []string{"12345", "item-ABC_123", "a.b:c@d"} {
		scan := newPersonalPlanScan(scanPolicy(2, 10, 10))
		if err := scan.Add(personalCandidatePage{RequestedStart: 0, Items: scanItems(id)}); err != nil {
			t.Fatalf("Id %q: %v", id, err)
		}
	}
}
