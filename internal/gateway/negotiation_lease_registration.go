package gateway

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
)

const negotiationLeaseRollbackTimeout = 2 * time.Second

type negotiationUpstreamResponse struct {
	Response     *http.Response
	Registration *negotiationLeaseRegistration
}

type negotiationLeaseRegistration struct {
	mu          sync.Mutex
	pending     bool
	registry    MediaLeaseRegistry
	owner       string
	selectors   negotiationSelectorSet
	cleanup     bool
	ctx         context.Context
	snapshot    upstreamRequestSnapshot
	closeStream func(context.Context, upstreamRequestSnapshot, PlaySessionID, LiveStreamID) error
}

func newNegotiationLeaseRegistration(registry MediaLeaseRegistry, owner string, selectors negotiationSelectorSet, operation routeclass.Operation, ctx context.Context, snapshot upstreamRequestSnapshot, closeStream func(context.Context, upstreamRequestSnapshot, PlaySessionID, LiveStreamID) error) *negotiationLeaseRegistration {
	return &negotiationLeaseRegistration{
		pending:     true,
		registry:    registry,
		owner:       owner,
		selectors:   cloneNegotiationSelectors(selectors),
		cleanup:     operation == routeclass.OperationPlaybackInfo || operation == routeclass.OperationLiveStreamOpen,
		ctx:         ctx,
		snapshot:    snapshot,
		closeStream: closeStream,
	}
}

func (r *negotiationLeaseRegistration) Commit() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if !r.pending {
		r.mu.Unlock()
		return nil
	}
	if r.registry == nil {
		r.pending = false
		r.mu.Unlock()
		r.rollbackCleanup()
		return fmt.Errorf("%w: lease registry unavailable", ErrStoreUnavailable)
	}
	err := r.registry.RegisterAll(r.owner, r.selectors.PlaySessionIDs, r.selectors.LiveStreamIDs)
	r.pending = false
	r.mu.Unlock()
	if err != nil {
		r.rollbackCleanup()
	}
	return err
}

func (r *negotiationLeaseRegistration) Rollback() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.pending {
		r.mu.Unlock()
		return
	}
	r.pending = false
	r.mu.Unlock()
	r.rollbackCleanup()
}

func (r *negotiationLeaseRegistration) Close() {
	r.Rollback()
}

func (r *negotiationLeaseRegistration) Selectors() negotiationSelectorSet {
	if r == nil {
		return negotiationSelectorSet{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneNegotiationSelectors(r.selectors)
}

func (r *negotiationLeaseRegistration) rollbackCleanup() {
	if !r.cleanup || len(r.selectors.LiveStreamIDs) == 0 || r.closeStream == nil {
		return
	}
	base := r.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(base), negotiationLeaseRollbackTimeout)
	defer cancel()
	var play PlaySessionID
	if len(r.selectors.PlaySessionIDs) != 0 {
		play = r.selectors.PlaySessionIDs[0]
	}
	for _, live := range r.selectors.LiveStreamIDs {
		_ = r.closeStream(ctx, r.snapshot, play, live)
	}
}

func cloneNegotiationSelectors(selectors negotiationSelectorSet) negotiationSelectorSet {
	return negotiationSelectorSet{
		PlaySessionIDs: append([]PlaySessionID(nil), selectors.PlaySessionIDs...),
		LiveStreamIDs:  append([]LiveStreamID(nil), selectors.LiveStreamIDs...),
	}
}
