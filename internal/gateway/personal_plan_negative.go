package gateway

import (
	"context"
	"fmt"
)

// executeNegativePersonalPlan evaluates a negative personal query against a
// single, scoped state snapshot and the complete upstream candidate set.
func executeNegativePersonalPlan(ctx context.Context, source *personalPlanSource, plan personalPlan) (personalPlanResult, error) {
	if plan.Kind != personalPlanNegative {
		return personalPlanResult{}, fmt.Errorf("negative personal executor requires negative plan")
	}
	if plan.Shape != personalShapeQueryResult {
		return personalPlanResult{}, fmt.Errorf("negative personal executor requires QueryResult shape")
	}
	if source == nil {
		return personalPlanResult{}, fmt.Errorf("negative personal executor requires source")
	}

	snapshot, err := source.snapshot(ctx)
	if err != nil {
		return personalPlanResult{}, err
	}
	scan := newPersonalPlanScan(plan.Scan)
	for !scan.Complete() {
		page, err := source.fetchCandidatePage(ctx, plan, scan.NextStart(), plan.Scan.PageSize)
		if err != nil {
			return personalPlanResult{}, err
		}
		if err := scan.Add(page); err != nil {
			return personalPlanResult{}, err
		}
	}

	joined, err := joinPersonalCandidates(scan.Candidates(), snapshot.States, false)
	if err != nil {
		return personalPlanResult{}, err
	}
	filtered := make([]resolvedPersonalItem, 0, len(joined))
	for _, item := range joined {
		if personalStateMatches(item.state, plan.Predicates) {
			filtered = append(filtered, item)
		}
	}
	sortPersonalPlanItems(filtered, plan.Sort)
	total := len(filtered)
	return personalPlanResult{
		Items:      pagePersonalPlanItems(filtered, plan.Page),
		Total:      &total,
		StartIndex: plan.Page.Start,
	}, nil
}
