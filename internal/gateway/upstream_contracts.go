package gateway

import (
	"context"
	"net/http"

	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
)

type upstreamPurpose uint8

const (
	upstreamPurposeMetadata upstreamPurpose = iota + 1
	upstreamPurposeMedia
	upstreamPurposeNegotiation
	upstreamPurposeManagedAuth
	upstreamPurposeLegacy
)

func (p upstreamPurpose) String() string {
	switch p {
	case upstreamPurposeMetadata:
		return "metadata"
	case upstreamPurposeMedia:
		return "media"
	case upstreamPurposeNegotiation:
		return "negotiation"
	case upstreamPurposeManagedAuth:
		return "managed_auth"
	case upstreamPurposeLegacy:
		return "legacy"
	default:
		return "unknown"
	}
}

// upstreamHTTPRequest carries the authenticated request context for one egress attempt.
type upstreamRefreshResult struct {
	Confirmed bool
	Err       error
}

type upstreamHTTPRequest struct {
	Request       *http.Request
	Route         PathPolicyDecision
	Session       *Session
	Snapshot      upstreamRequestSnapshot
	refreshResult func(upstreamRefreshResult)
}

func (r upstreamHTTPRequest) notifyRefreshResult(result upstreamRefreshResult) {
	if r.refreshResult != nil {
		r.refreshResult(result)
	}
}

type metadataUpstreamRequest struct {
	upstreamHTTPRequest
	Ownership   routeclass.Ownership
	Internal    bool
	Public      bool
	SnapshotRef *upstreamRequestSnapshot
}
type mediaUpstreamRequest struct {
	upstreamHTTPRequest
	Internal    bool
	Anonymous   bool
	SnapshotRef *upstreamRequestSnapshot
}
type negotiationUpstreamRequest struct {
	upstreamHTTPRequest
	SnapshotRef *upstreamRequestSnapshot
}
type legacyUpstreamRequest struct {
	upstreamHTTPRequest
	SnapshotRef *upstreamRequestSnapshot
}

// MetadataUpstream owns metadata GET/HEAD egress and its redirect policy.
type MetadataUpstream interface {
	RoundTripMetadata(metadataUpstreamRequest) (*http.Response, error)
}

// MediaUpstream owns media GET/HEAD and playback negotiation egress.
type MediaUpstream interface {
	RoundTripMedia(mediaUpstreamRequest) (*http.Response, error)
	RoundTripNegotiation(negotiationUpstreamRequest) (*http.Response, error)
}

type managedAuthProbeRequest struct {
	Context  context.Context
	Snapshot UpstreamRuntime
}

type managedAuthLoginRequest struct {
	Context context.Context
	Runtime UpstreamRuntime
}

type managedAuthLogoutRequest struct {
	Context  context.Context
	Snapshot upstreamRequestSnapshot
}

// ManagedAuthUpstream owns managed login, probe, refresh, and cleanup egress.
type ManagedAuthUpstream interface {
	Ensure(context.Context) (*UpstreamRuntime, error)
	Refresh(context.Context, string) (*UpstreamRuntime, error)
	Probe(managedAuthProbeRequest) (UpstreamServerInfoUpdate, error)
	Login(managedAuthLoginRequest) (UpstreamAuthUpdate, error)
	Logout(managedAuthLogoutRequest) error
}

// LegacyHTTPUpstream owns only requests classified as legacy HTTP egress.
type LegacyHTTPUpstream interface {
	RoundTripLegacy(legacyUpstreamRequest) (*http.Response, error)
}
