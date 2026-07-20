package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
)

type countingLeaseRegistry struct {
	MediaLeaseRegistry
	registerCalls int
}

func (r *countingLeaseRegistry) RegisterAll(owner string, play []PlaySessionID, live []LiveStreamID) error {
	r.registerCalls++
	return r.MediaLeaseRegistry.RegisterAll(owner, play, live)
}

func TestNegotiationLeaseRegistrationCommitIsDeferredAndIdempotent(t *testing.T) {
	registry := &countingLeaseRegistry{MediaLeaseRegistry: NewMediaLeaseRegistry(nil)}
	registration := newNegotiationLeaseRegistration(
		registry,
		"owner",
		negotiationSelectorSet{PlaySessionIDs: []PlaySessionID{"play-1"}, LiveStreamIDs: []LiveStreamID{"live-1"}},
		routeclass.OperationPlaybackInfo,
		context.Background(),
		upstreamRequestSnapshot{},
		nil,
	)
	defer registration.Close()

	if registry.registerCalls != 0 {
		t.Fatalf("register calls before commit = %d", registry.registerCalls)
	}
	if _, err := registry.Validate("owner", "play-1", "live-1", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lease before commit = %v", err)
	}
	if err := registration.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := registration.Commit(); err != nil {
		t.Fatalf("second commit = %v", err)
	}
	registration.Rollback()
	registration.Close()
	if registry.registerCalls != 1 {
		t.Fatalf("register calls after repeated commit = %d", registry.registerCalls)
	}
	if _, err := registry.Validate("owner", "play-1", "live-1", time.Time{}); err != nil {
		t.Fatalf("committed lease = %v", err)
	}
}

func TestNegotiationLeaseRegistrationRollbackCleanupLifecycle(t *testing.T) {
	for _, tt := range []struct {
		name      string
		operation routeclass.Operation
	}{
		{name: "PlaybackInfo", operation: routeclass.OperationPlaybackInfo},
		{name: "LiveStreamOpen", operation: routeclass.OperationLiveStreamOpen},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base, cancel := context.WithCancel(context.Background())
			cancel()
			var closes int
			registration := newNegotiationLeaseRegistration(
				NewMediaLeaseRegistry(nil),
				"owner",
				negotiationSelectorSet{PlaySessionIDs: []PlaySessionID{"play-1"}, LiveStreamIDs: []LiveStreamID{"live-1"}},
				tt.operation,
				base,
				upstreamRequestSnapshot{},
				func(ctx context.Context, _ upstreamRequestSnapshot, play PlaySessionID, live LiveStreamID) error {
					closes++
					if ctx.Err() != nil || play != "play-1" || live != "live-1" {
						t.Fatalf("cleanup context/selectors = %v/%q/%q", ctx.Err(), play, live)
					}
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > negotiationLeaseRollbackTimeout {
						t.Fatalf("cleanup deadline = %v, present=%v", deadline, ok)
					}
					return nil
				},
			)
			registration.Rollback()
			registration.Rollback()
			registration.Close()
			if err := registration.Commit(); err != nil {
				t.Fatalf("commit after rollback = %v", err)
			}
			if closes != 1 {
				t.Fatalf("cleanup closes = %d", closes)
			}
			if owners := registration.registry.Owners(); len(owners) != 0 {
				t.Fatalf("rollback retained owners = %#v", owners)
			}
		})
	}
}

func TestNegotiationLeaseRegistrationMediaInfoRollbackDoesNotCloseExistingStream(t *testing.T) {
	var closes int
	registration := newNegotiationLeaseRegistration(
		NewMediaLeaseRegistry(nil),
		"owner",
		negotiationSelectorSet{PlaySessionIDs: []PlaySessionID{"play-1"}, LiveStreamIDs: []LiveStreamID{"existing-live"}},
		routeclass.OperationLiveStreamMediaInfo,
		context.Background(),
		upstreamRequestSnapshot{},
		func(context.Context, upstreamRequestSnapshot, PlaySessionID, LiveStreamID) error {
			closes++
			return nil
		},
	)
	registration.Close()
	registration.Rollback()
	if closes != 0 {
		t.Fatalf("MediaInfo rollback closes = %d", closes)
	}
	if owners := registration.registry.Owners(); len(owners) != 0 {
		t.Fatalf("MediaInfo rollback retained owners = %#v", owners)
	}
}

func TestNegotiationLeaseRegistrationOnlyFinalRetryCanCommit(t *testing.T) {
	firstBody := &adapterCloseCountingBody{Reader: bytes.NewBufferString(`{"PlaySessionId":"discarded-play","LiveStreamId":"discarded-live"}`)}
	finalBody := &adapterCloseCountingBody{Reader: bytes.NewBufferString(`{"PlaySessionId":"final-play","LiveStreamId":"final-live"}`)}
	var calls int
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: firstBody, Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: finalBody, Request: req}, nil
	})}
	registry := NewMediaLeaseRegistry(nil)
	adapter := newMediaUpstream(client, func(_ context.Context, snapshot upstreamRequestSnapshot) (upstreamRequestSnapshot, bool, error) {
		snapshot.token = "refreshed-token"
		return snapshot, true, nil
	}, registry, nil)
	request := httptest.NewRequest(http.MethodPost, "http://gateway.test/LiveStreams/Open", http.NoBody)
	result, err := adapter.RoundTripNegotiation(negotiationUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{
		Request:  request,
		Session:  &Session{GatewayTokenHash: "owner", SyntheticUserID: "gateway-user"},
		Snapshot: testUpstreamSnapshot("http://backend.invalid"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Response.Body.Close()
	defer result.Registration.Close()
	if calls != 2 || firstBody.closes != 1 {
		t.Fatalf("retry calls/first closes = %d/%d", calls, firstBody.closes)
	}
	selectors := result.Registration.Selectors()
	if len(selectors.PlaySessionIDs) != 1 || selectors.PlaySessionIDs[0] != "final-play" || len(selectors.LiveStreamIDs) != 1 || selectors.LiveStreamIDs[0] != "final-live" {
		t.Fatalf("final selectors = %#v", selectors)
	}
	if _, err := registry.Validate("owner", "discarded-play", "discarded-live", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("discarded response registered = %v", err)
	}
	if err := result.Registration.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Validate("owner", "final-play", "final-live", time.Time{}); err != nil {
		t.Fatalf("final response not registered = %v", err)
	}
	_, _ = io.ReadAll(result.Response.Body)
	_ = result.Response.Body.Close()
	_ = result.Response.Body.Close()
	if finalBody.closes != 1 {
		t.Fatalf("final response closes = %d", finalBody.closes)
	}
}
