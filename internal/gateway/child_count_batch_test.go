package gateway

import (
	"context"
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
