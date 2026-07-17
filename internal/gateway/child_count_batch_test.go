package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type countingChildCountStore struct {
	*MemoryStore
	mu          sync.Mutex
	batchCalls  int
	batchItems  int
	singleCalls int
}

func (c *countingChildCountStore) SaveItemChildCounts(ctx context.Context, counts []ItemChildCount) error {
	c.mu.Lock()
	c.batchCalls++
	c.batchItems += len(counts)
	c.mu.Unlock()
	return c.MemoryStore.SaveItemChildCounts(ctx, counts)
}

func (c *countingChildCountStore) SaveItemChildCount(ctx context.Context, count ItemChildCount) error {
	c.mu.Lock()
	c.singleCalls++
	c.mu.Unlock()
	return c.MemoryStore.SaveItemChildCount(ctx, count)
}

func TestLearnChildCountsFromItemsBatchesSaves(t *testing.T) {
	store := &countingChildCountStore{MemoryStore: NewMemoryStore()}
	session := &Session{SyntheticUserID: "user-1"}
	items := []map[string]any{
		{"Id": "a", "ChildCount": 3},
		{"Id": "b", "RecursiveItemCount": 5},
		{"Id": "c", "Count": 2},
		{"Id": "", "ChildCount": 9},
		{"Id": "d", "ChildCount": 0},
	}

	learnChildCountsFromItems(context.Background(), store, session, items)

	if store.batchCalls != 1 {
		t.Fatalf("SaveItemChildCounts calls = %d, want 1", store.batchCalls)
	}
	if store.batchItems != 3 {
		t.Fatalf("SaveItemChildCounts items = %d, want 3", store.batchItems)
	}
	if store.singleCalls != 0 {
		t.Fatalf("SaveItemChildCount calls = %d, want 0", store.singleCalls)
	}

	counts, err := store.ListItemChildCounts(context.Background(), []string{"a", "b", "c", "d"})
	if err != nil {
		t.Fatalf("list child counts: %v", err)
	}
	if len(counts) != 3 || counts["a"].ChildCount != 3 || counts["b"].ChildCount != 5 || counts["c"].ChildCount != 2 {
		t.Fatalf("unexpected persisted counts: %#v", counts)
	}
}

func TestLearnChildCountsFromItemsDedupesByItemID(t *testing.T) {
	store := &countingChildCountStore{MemoryStore: NewMemoryStore()}
	session := &Session{SyntheticUserID: "user-1"}
	items := []map[string]any{
		{"Id": "a", "ChildCount": 3},
		{"Id": "a", "ChildCount": 7}, // last non-zero wins
		{"Id": "b", "ChildCount": 2},
		{"Id": "b", "ChildCount": 0}, // zero ignored; prior 2 kept via last non-zero
		{"Id": "a", "RecursiveItemCount": 9},
	}

	learnChildCountsFromItems(context.Background(), store, session, items)

	if store.batchCalls != 1 {
		t.Fatalf("SaveItemChildCounts calls = %d, want 1", store.batchCalls)
	}
	if store.batchItems != 2 {
		t.Fatalf("SaveItemChildCounts items = %d, want 2 (deduped)", store.batchItems)
	}

	counts, err := store.ListItemChildCounts(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("list child counts: %v", err)
	}
	if counts["a"].ChildCount != 9 {
		t.Fatalf("a ChildCount = %d, want 9 (last non-zero)", counts["a"].ChildCount)
	}
	if counts["b"].ChildCount != 2 {
		t.Fatalf("b ChildCount = %d, want 2", counts["b"].ChildCount)
	}
}

// bestEffortChildCountStore mirrors pbstore.SaveItemChildCounts failure policy:
// one entry failure must not prevent later valid entries from being written.
type bestEffortChildCountStore struct {
	*MemoryStore
	failIDs map[string]bool
}

func (s *bestEffortChildCountStore) SaveItemChildCounts(ctx context.Context, counts []ItemChildCount) error {
	var firstErr error
	for _, count := range counts {
		if s.failIDs[count.ItemID] {
			if firstErr == nil {
				firstErr = errors.New("forced entry failure")
			}
			continue
		}
		if err := s.MemoryStore.SaveItemChildCount(ctx, count); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func TestSaveItemChildCountsBestEffortContinuesAfterEntryFailure(t *testing.T) {
	store := &bestEffortChildCountStore{
		MemoryStore: NewMemoryStore(),
		failIDs:     map[string]bool{"bad": true},
	}
	err := store.SaveItemChildCounts(context.Background(), []ItemChildCount{
		{ItemID: "bad", ChildCount: 1},
		{ItemID: "good-1", ChildCount: 3},
		{ItemID: "good-2", ChildCount: 5},
	})
	if err == nil {
		t.Fatal("expected first entry failure to be returned")
	}
	counts, listErr := store.ListItemChildCounts(context.Background(), []string{"bad", "good-1", "good-2"})
	if listErr != nil {
		t.Fatalf("list: %v", listErr)
	}
	if _, ok := counts["bad"]; ok {
		t.Fatalf("failed entry should not be persisted: %#v", counts)
	}
	if counts["good-1"].ChildCount != 3 || counts["good-2"].ChildCount != 5 {
		t.Fatalf("later valid entries must still persist: %#v", counts)
	}
}
