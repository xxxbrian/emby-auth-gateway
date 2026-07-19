package gateway

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestExecuteNegativePersonalPlanUsesCompleteLocalResult(t *testing.T) {
	fake := &personalPlanSourceMetadataFake{responses: []personalPlanSourceMetadataResponse{
		{status: http.StatusOK, body: `{"Items":[{"Id":"b"},{"Id":"a"}],"TotalRecordCount":4}`},
		{status: http.StatusOK, body: `{"Items":[{"Id":"d"},{"Id":"c"}],"TotalRecordCount":4}`},
	}}
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore(), states: []PlaybackState{{ItemID: "a", Played: true}}}
	source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
	limit := 1
	plan := personalPlan{Kind: personalPlanNegative, Shape: personalShapeQueryResult, Predicates: personalPredicates{Played: personalTruthFalse}, Page: personalPageSpec{Start: 1, Limit: &limit}, Scan: scanPolicy(2, 10, 10)}

	result, err := executeNegativePersonalPlan(context.Background(), source, plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total == nil || *result.Total != 3 || result.StartIndex != 1 || len(result.Items) != 1 {
		t.Fatalf("result = %#v, want total 3, start 1, one item", result)
	}
	if id, _ := personalItemID(result.Items[0].item); id != "c" {
		t.Fatalf("paged item id = %q, want c", id)
	}
	if fake.calls != 2 {
		t.Fatalf("metadata calls = %d, want complete scan of 2 pages", fake.calls)
	}
}

func TestExecuteNegativePersonalPlanZeroStateAndOrphanState(t *testing.T) {
	fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: `{"Items":[{"Id":"orphan"},{"Id":"unstated"}],"TotalRecordCount":2}`}
	orphanedAt := time.Unix(123, 0).UTC()
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore(), states: []PlaybackState{{ItemID: "orphan", Played: true, OrphanedAt: &orphanedAt}}}
	source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
	plan := personalPlan{Kind: personalPlanNegative, Shape: personalShapeQueryResult, Predicates: personalPredicates{Played: personalTruthTrue}, Scan: scanPolicy(2, 2, 2)}

	result, err := executeNegativePersonalPlan(context.Background(), source, plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total == nil || *result.Total != 1 || len(result.Items) != 1 {
		t.Fatalf("result = %#v, want only orphan state", result)
	}
	if id, _ := personalItemID(result.Items[0].item); id != "orphan" {
		t.Fatalf("item id = %q, want orphan", id)
	}
	if !result.Items[0].state.Played || result.Items[0].state.OrphanedAt == nil || !result.Items[0].state.OrphanedAt.Equal(orphanedAt) {
		t.Fatalf("joined state = %#v, want played orphaned local state", result.Items[0].state)
	}
}

func TestExecuteNegativePersonalPlanLimitZeroScansBeforePaging(t *testing.T) {
	fake := &personalPlanSourceMetadataFake{responses: []personalPlanSourceMetadataResponse{
		{status: http.StatusOK, body: `{"Items":[{"Id":"b"},{"Id":"a"}],"TotalRecordCount":4}`},
		{status: http.StatusOK, body: `{"Items":[{"Id":"d"},{"Id":"c"}],"TotalRecordCount":4}`},
	}}
	source, _ := newPersonalPlanSourceTestSource(t, fake)
	limit := 0
	plan := personalPlan{Kind: personalPlanNegative, Shape: personalShapeQueryResult, Page: personalPageSpec{Start: 1, Limit: &limit}, Scan: scanPolicy(2, 10, 10)}

	result, err := executeNegativePersonalPlan(context.Background(), source, plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total == nil || *result.Total != 4 || len(result.Items) != 0 || result.StartIndex != 1 {
		t.Fatalf("result = %#v, want total 4, empty items, start 1", result)
	}
	if fake.calls != 2 {
		t.Fatalf("metadata calls = %d, want full two-page scan", fake.calls)
	}
}

func TestExecuteNegativePersonalPlanNoTotalShortTerminalIsExact(t *testing.T) {
	fake := &personalPlanSourceMetadataFake{responses: []personalPlanSourceMetadataResponse{
		{status: http.StatusOK, body: `{"Items":[{"Id":"b"},{"Id":"a"}]}`},
		{status: http.StatusOK, body: `{"Items":[{"Id":"c"}]}`},
	}}
	source, _ := newPersonalPlanSourceTestSource(t, fake)
	plan := personalPlan{Kind: personalPlanNegative, Shape: personalShapeQueryResult, Scan: scanPolicy(2, 10, 10)}

	result, err := executeNegativePersonalPlan(context.Background(), source, plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total == nil || *result.Total != 3 || len(result.Items) != 3 {
		t.Fatalf("result = %#v, want exact total 3 and three items", result)
	}
	if fake.calls != 2 {
		t.Fatalf("metadata calls = %d, want two pages", fake.calls)
	}
}

func TestExecuteNegativePersonalPlanRejectsWrongContract(t *testing.T) {
	source := &personalPlanSource{}
	for _, plan := range []personalPlan{
		{Kind: personalPlanPositive, Shape: personalShapeQueryResult},
		{Kind: personalPlanNegative, Shape: personalShapeArray},
	} {
		if _, err := executeNegativePersonalPlan(context.Background(), source, plan); err == nil {
			t.Fatal("expected contract error")
		}
	}
}

func TestExecuteNegativePersonalPlanPropagatesScanErrors(t *testing.T) {
	fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: `{"Items":[{"Id":"a"},{"Id":"a"}]}`}
	source, _ := newPersonalPlanSourceTestSource(t, fake)
	plan := personalPlan{Kind: personalPlanNegative, Shape: personalShapeQueryResult, Scan: scanPolicy(2, 2, 2)}
	_, err := executeNegativePersonalPlan(context.Background(), source, plan)
	if err == nil || errors.Is(err, ErrPersonalScanIncomplete) {
		t.Fatalf("err = %v, want proven scanner error", err)
	}
}
