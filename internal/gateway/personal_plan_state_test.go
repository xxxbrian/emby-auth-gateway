package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func executorState(id string) PlaybackState {
	return PlaybackState{ItemID: id, GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user"}
}

func executorSource(t *testing.T, states []PlaybackState, responses ...personalPlanSourceMetadataResponse) (*personalPlanSource, *personalPlanSourceStore) {
	t.Helper()
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore(), states: states}
	fake := &personalPlanSourceMetadataFake{responses: responses}
	source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
	return source, store
}

func executorResponse(t *testing.T, ids ...string) personalPlanSourceMetadataResponse {
	t.Helper()
	return personalPlanSourceMetadataResponse{
		status: http.StatusOK,
		body:   personalPlanSourceJSON(t, map[string]any{"Items": personalPlanSourceItems(ids, false)}),
	}
}

func executorPlan(kind personalPlanKind) personalPlan {
	return personalPlan{Kind: kind, Shape: personalShapeQueryResult, Scan: personalScanPolicy{MaxItems: 10_000}}
}

func TestExecutePositivePersonalPlanIntersectsAllPredicatesAndPagesAfterTotal(t *testing.T) {
	liked := true
	now := time.Now().UTC()
	states := []PlaybackState{
		func() PlaybackState {
			s := executorState("match")
			s.IsFavorite = true
			s.PlaybackPositionTicks = 10
			s.Likes = &liked
			s.LastPlayedDate = &now
			return s
		}(),
		func() PlaybackState {
			s := executorState("wrong-played")
			s.Played = true
			s.IsFavorite = true
			s.PlaybackPositionTicks = 10
			s.Likes = &liked
			return s
		}(),
		func() PlaybackState {
			s := executorState("wrong-rating")
			s.IsFavorite = true
			s.PlaybackPositionTicks = 10
			return s
		}(),
	}
	source, store := executorSource(t, states, executorResponse(t, "match"))
	plan := executorPlan(personalPlanPositive)
	plan.Predicates = personalPredicates{Played: personalTruthFalse, Favorite: personalTruthTrue, Resumable: personalTruthTrue, Rating: personalRatingLiked}
	limit := 0
	plan.Page = personalPageSpec{Start: 1, Limit: &limit}
	result, err := executePositivePersonalPlan(context.Background(), source, plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total == nil || *result.Total != 1 || result.StartIndex != 1 || len(result.Items) != 0 {
		t.Fatalf("result = %#v", result)
	}
	if store.listCalls != 1 || store.findCalls != 0 || len(store.saves) != 1 {
		t.Fatalf("list=%d find=%d saves=%d", store.listCalls, store.findCalls, len(store.saves))
	}
}

func TestExecutePositivePersonalPlanDeterministicRecencyAndTie(t *testing.T) {
	date := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	updated := date.Add(time.Hour)
	states := []PlaybackState{}
	for _, id := range []string{"b", "a"} {
		s := executorState(id)
		s.LastPlayedDate = &date
		s.UpdatedAt = updated
		states = append(states, s)
	}
	source, _ := executorSource(t, states, executorResponse(t, "a", "b"))
	result, err := executePositivePersonalPlan(context.Background(), source, executorPlan(personalPlanPositive))
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{result.Items[0].state.ItemID, result.Items[1].state.ItemID}; !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("order = %v", got)
	}
}

func TestExecuteResumePersonalPlanDoesNotGroupSeries(t *testing.T) {
	first := executorState("episode-1")
	second := executorState("episode-2")
	first.PlaybackPositionTicks = 10
	second.PlaybackPositionTicks = 20
	first.SeriesID, second.SeriesID = "series", "series"
	source, _ := executorSource(t, []PlaybackState{first, second}, executorResponse(t, "episode-1", "episode-2"))
	result, err := executeResumePersonalPlan(context.Background(), source, executorPlan(personalPlanResume))
	if err != nil || len(result.Items) != 2 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestExecuteStatePersonalPlanRepairsOrphanButStructuralMissIsNotOrphaned(t *testing.T) {
	orphaned := time.Now().UTC().Add(-time.Hour)
	state := executorState("orphan")
	state.OrphanedAt = &orphaned
	source, store := executorSource(t, []PlaybackState{state}, executorResponse(t, "orphan"), executorResponse(t))
	plan := executorPlan(personalPlanPositive)
	plan.Refinement = url.Values{}
	plan.Refinement.Set("ParentId", "different")
	result, err := executePositivePersonalPlan(context.Background(), source, plan)
	if err != nil || result.Total == nil || *result.Total != 0 || len(store.saves) != 1 || store.saves[0].OrphanedAt != nil {
		t.Fatalf("result=%#v saves=%#v err=%v", result, store.saves, err)
	}
}

func TestExecuteStatePersonalPlanBoundAndFailures(t *testing.T) {
	states := make([]PlaybackState, 10_001)
	for i := range states {
		states[i] = executorState("item-" + strconv.Itoa(i))
	}
	source, store := executorSource(t, states)
	_, err := executePositivePersonalPlan(context.Background(), source, executorPlan(personalPlanPositive))
	if !errors.Is(err, ErrPersonalScanIncomplete) || store.listCalls != 1 {
		t.Fatalf("err=%v list=%d", err, store.listCalls)
	}

	resolutionErr := errors.New("resolution failed")
	source, _ = executorSource(t, []PlaybackState{executorState("item")}, personalPlanSourceMetadataResponse{err: resolutionErr})
	_, err = executePositivePersonalPlan(context.Background(), source, executorPlan(personalPlanPositive))
	if !errors.Is(err, resolutionErr) {
		t.Fatalf("resolution error = %v", err)
	}
}

func TestExecuteStatePersonalPlanStoreAndRefinementFailures(t *testing.T) {
	storeErr := errors.New("store failed")
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore(), listErr: storeErr}
	fake := &personalPlanSourceMetadataFake{}
	source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
	if _, err := executePositivePersonalPlan(context.Background(), source, executorPlan(personalPlanPositive)); !errors.Is(err, storeErr) {
		t.Fatalf("store error = %v", err)
	}

	refinementErr := errors.New("refinement failed")
	source, _ = executorSource(t, []PlaybackState{executorState("item")}, executorResponse(t, "item"), personalPlanSourceMetadataResponse{err: refinementErr})
	plan := executorPlan(personalPlanPositive)
	plan.Refinement = url.Values{}
	plan.Refinement.Set("ParentId", "parent")
	if _, err := executePositivePersonalPlan(context.Background(), source, plan); !errors.Is(err, refinementErr) {
		t.Fatalf("refinement error = %v", err)
	}
}

func TestExecuteStatePersonalPlanEmptyAndContractValidation(t *testing.T) {
	source, store := executorSource(t, nil)
	result, err := executePositivePersonalPlan(context.Background(), source, executorPlan(personalPlanPositive))
	if err != nil || result.Total == nil || *result.Total != 0 || len(result.Items) != 0 || store.listCalls != 1 {
		t.Fatalf("result=%#v err=%v list=%d", result, err, store.listCalls)
	}
	if _, err := executePositivePersonalPlan(context.Background(), source, executorPlan(personalPlanResume)); err == nil {
		t.Fatal("wrong kind accepted")
	}
	badShape := executorPlan(personalPlanPositive)
	badShape.Shape = personalShapeArray
	if _, err := executePositivePersonalPlan(context.Background(), source, badShape); err == nil {
		t.Fatal("wrong shape accepted")
	}
	if _, err := executePositivePersonalPlan(context.Background(), nil, executorPlan(personalPlanPositive)); err == nil {
		t.Fatal("nil source accepted")
	}
}
