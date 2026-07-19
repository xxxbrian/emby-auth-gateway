package gateway

import (
	"context"
	"fmt"
	"sort"
)

func executePositivePersonalPlan(ctx context.Context, source *personalPlanSource, plan personalPlan) (personalPlanResult, error) {
	if plan.Kind != personalPlanPositive {
		return personalPlanResult{}, fmt.Errorf("positive executor received plan kind %d", plan.Kind)
	}
	return executeStatePersonalPlan(ctx, plan, source, func(state PlaybackState) bool {
		return personalStateMatches(state, plan.Predicates)
	})
}

func executeResumePersonalPlan(ctx context.Context, source *personalPlanSource, plan personalPlan) (personalPlanResult, error) {
	if plan.Kind != personalPlanResume {
		return personalPlanResult{}, fmt.Errorf("resume executor received plan kind %d", plan.Kind)
	}
	return executeStatePersonalPlan(ctx, plan, source, func(state PlaybackState) bool {
		return !state.Played && state.PlaybackPositionTicks > 0 && personalStateMatches(state, plan.Predicates)
	})
}

func executeStatePersonalPlan(ctx context.Context, plan personalPlan, source *personalPlanSource, matches func(PlaybackState) bool) (personalPlanResult, error) {
	if source == nil {
		return personalPlanResult{}, fmt.Errorf("personal plan source is nil")
	}
	if plan.Shape != personalShapeQueryResult {
		return personalPlanResult{}, fmt.Errorf("personal state executor requires QueryResult shape")
	}

	snapshot, err := source.snapshot(ctx)
	if err != nil {
		return personalPlanResult{}, fmt.Errorf("snapshot personal state: %w", err)
	}
	ids := make([]string, 0, len(snapshot.States))
	for id, state := range snapshot.States {
		if id == "" || state.ItemID != id || !matches(state) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return personalPlanResult{Items: []resolvedPersonalItem{}, Total: intPointer(0), StartIndex: plan.Page.Start}, nil
	}
	maxItems := plan.Scan.MaxItems
	if maxItems <= 0 {
		maxItems = personalPlanScanMaxItems
	}
	if maxItems > personalPlanScanMaxItems {
		maxItems = personalPlanScanMaxItems
	}
	if len(ids) > maxItems {
		return personalPlanResult{}, fmt.Errorf("%w: %d matching state IDs exceed maximum of %d", ErrPersonalScanIncomplete, len(ids), maxItems)
	}

	resolved, err := source.resolveIDs(ctx, plan, snapshot, ids)
	if err != nil {
		return personalPlanResult{}, fmt.Errorf("resolve personal plan IDs: %w", err)
	}
	refined, err := source.refineResolved(ctx, plan, resolved)
	if err != nil {
		return personalPlanResult{}, fmt.Errorf("refine personal plan items: %w", err)
	}

	if len(plan.Sort) == 0 {
		sortPersonalPlanItems(refined, []personalSortTerm{
			{Name: "LastPlayedDate", Source: personalSortLocal, Direction: personalSortDescending},
			{Name: "UpdatedAt", Source: personalSortLocal, Direction: personalSortDescending},
		})
	} else {
		sortPersonalPlanItems(refined, plan.Sort)
	}
	total := len(refined)
	paged := pagePersonalPlanItems(refined, plan.Page)
	return personalPlanResult{Items: paged, Total: &total, StartIndex: plan.Page.Start}, nil
}

func intPointer(value int) *int { return &value }
