package gateway

import (
	"errors"
	"net/url"
)

type personalPlanKind uint8

const (
	personalPlanPositive personalPlanKind = iota
	personalPlanNegative
	personalPlanResume
	personalPlanNextUp
	personalPlanLatest
	personalPlanPassthrough
)

type personalRouteKind uint8

const (
	personalRouteItems personalRouteKind = iota
	personalRouteShowItems
	personalRouteResume
	personalRouteNextUp
	personalRouteLatest
)

type personalTruth uint8

const (
	personalTruthAny personalTruth = iota
	personalTruthTrue
	personalTruthFalse
)

type personalRating uint8

const (
	personalRatingAny personalRating = iota
	personalRatingLiked
	personalRatingDisliked
)

type personalPredicates struct {
	Played    personalTruth
	Favorite  personalTruth
	Resumable personalTruth
	Rating    personalRating
}

type personalPageSpec struct {
	Start int
	Limit *int
}

type personalScanPolicy struct {
	PageSize int
	MaxPages int
	MaxItems int
}

type personalGroupSpec struct {
	Items    bool
	Explicit bool
}

type personalSortSource uint8

const (
	personalSortMetadata personalSortSource = iota
	personalSortLocal
)

type personalSortDirection uint8

const (
	personalSortAscending personalSortDirection = iota
	personalSortDescending
)

type personalSortTerm struct {
	Name      string
	Source    personalSortSource
	Direction personalSortDirection
}

type personalResultShape uint8

const (
	personalShapeQueryResult personalResultShape = iota
	personalShapeArray
	personalShapePassthrough
)

type personalPlan struct {
	Kind       personalPlanKind
	Route      personalRouteKind
	Path       string
	Predicates personalPredicates
	Neutral    url.Values
	Refinement url.Values
	Projection url.Values
	Group      personalGroupSpec
	Sort       []personalSortTerm
	Page       personalPageSpec
	Scan       personalScanPolicy
	Shape      personalResultShape
}

type personalCandidatePage struct {
	Items          []map[string]any
	RequestedStart int
	ReturnedStart  *int
	Total          *int
	Terminal       bool
}

type personalPlanResult struct {
	Items      []resolvedPersonalItem
	Total      *int
	StartIndex int
}

var ErrPersonalScanIncomplete = errors.New("personal query scan incomplete")
