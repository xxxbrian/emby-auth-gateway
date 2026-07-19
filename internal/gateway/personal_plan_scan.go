package gateway

import "fmt"

// personalPlanScan validates and accumulates the upstream pages needed by a
// local personal query. It deliberately has no knowledge of HTTP or response
// shaping.
type personalPlanScan struct {
	policy    personalScanPolicy
	items     []map[string]any
	ids       map[string]struct{}
	nextStart int
	pageCount int
	total     *int
	complete  bool
}

func newPersonalPlanScan(policy personalScanPolicy) *personalPlanScan {
	return &personalPlanScan{policy: policy, ids: make(map[string]struct{})}
}

func (s *personalPlanScan) Add(page personalCandidatePage) error {
	if s.complete {
		return fmt.Errorf("personal scan received a page after completion")
	}
	if s.policy.PageSize <= 0 || s.policy.MaxPages <= 0 || s.policy.MaxItems <= 0 {
		return fmt.Errorf("invalid personal scan policy")
	}
	if page.RequestedStart != s.nextStart {
		return fmt.Errorf("personal scan requested start %d, expected %d", page.RequestedStart, s.nextStart)
	}
	if page.ReturnedStart != nil && *page.ReturnedStart != page.RequestedStart {
		return fmt.Errorf("personal scan returned start %d, requested %d", *page.ReturnedStart, page.RequestedStart)
	}
	if len(page.Items) > s.policy.PageSize {
		return fmt.Errorf("personal scan page has %d items, maximum is %d", len(page.Items), s.policy.PageSize)
	}
	if page.Total != nil {
		if *page.Total < 0 {
			return fmt.Errorf("personal scan total %d is negative", *page.Total)
		}
		if s.total != nil && *s.total != *page.Total {
			return fmt.Errorf("personal scan total changed from %d to %d", *s.total, *page.Total)
		}
	}
	prospectiveTotal := s.total
	if prospectiveTotal == nil && page.Total != nil {
		total := *page.Total
		prospectiveTotal = &total
	}
	prospectiveCount := len(s.items) + len(page.Items)
	if prospectiveCount > s.policy.MaxItems {
		return fmt.Errorf("%w: page would exceed maximum of %d items", ErrPersonalScanIncomplete, s.policy.MaxItems)
	}
	pageIDs := make(map[string]struct{}, len(page.Items))
	for index, item := range page.Items {
		if item == nil {
			return fmt.Errorf("personal scan item %d is null", index)
		}
		id, ok := item["Id"].(string)
		if !ok || id == "" {
			return fmt.Errorf("personal scan item %d has a missing or invalid Id", index)
		}
		if !safeItemID(id) {
			return fmt.Errorf("personal scan item %d has an unsafe Id", index)
		}
		if _, exists := s.ids[id]; exists {
			return fmt.Errorf("personal scan contains duplicate Id %q", id)
		}
		if _, exists := pageIDs[id]; exists {
			return fmt.Errorf("personal scan contains duplicate Id %q", id)
		}
		pageIDs[id] = struct{}{}
	}
	if prospectiveTotal != nil && prospectiveCount > *prospectiveTotal {
		return fmt.Errorf("personal scan consumed %d items, total is %d", prospectiveCount, *prospectiveTotal)
	}
	short := len(page.Items) < s.policy.PageSize
	complete := prospectiveTotal != nil && prospectiveCount == *prospectiveTotal
	if short && prospectiveTotal != nil && prospectiveCount < *prospectiveTotal {
		return fmt.Errorf("personal scan ended after %d items before total %d", prospectiveCount, *prospectiveTotal)
	}
	if short {
		complete = true
	} else if page.Terminal && !complete {
		return fmt.Errorf("personal scan claimed terminal full page without proving total")
	}

	for _, item := range page.Items {
		id := item["Id"].(string)
		s.ids[id] = struct{}{}
		s.items = append(s.items, item)
	}
	s.pageCount++
	s.nextStart += len(page.Items)
	s.total = prospectiveTotal
	s.complete = complete

	if !s.complete && (s.pageCount >= s.policy.MaxPages || prospectiveCount >= s.policy.MaxItems) {
		return fmt.Errorf("%w: scan requires another page after %d pages and %d items", ErrPersonalScanIncomplete, s.pageCount, prospectiveCount)
	}
	return nil
}

func (s *personalPlanScan) NextStart() int { return s.nextStart }

func (s *personalPlanScan) CandidateCount() int { return len(s.items) }

func (s *personalPlanScan) PageCount() int { return s.pageCount }

func (s *personalPlanScan) Total() (int, bool) {
	if s.total == nil {
		return 0, false
	}
	return *s.total, true
}

func (s *personalPlanScan) Complete() bool { return s.complete }

func (s *personalPlanScan) Candidates() []map[string]any { return s.items }
