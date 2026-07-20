package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
)

type downloadFallbackPortSpy struct {
	t                  *testing.T
	leases             MediaLeaseRegistry
	calls              []string
	mediaBodyClose     atomic.Int32
	mediaErr           error
	negotiationRefresh *upstreamRefreshResult
	mediaRefresh       *upstreamRefreshResult
	cancelPhase        string
	cancel             context.CancelFunc
}

func (s *downloadFallbackPortSpy) RoundTripNegotiation(in negotiationUpstreamRequest) (negotiationUpstreamResponse, error) {
	s.calls = append(s.calls, "negotiation")
	if s.cancelPhase == "negotiation" && s.cancel != nil {
		s.cancel()
	}
	if s.negotiationRefresh != nil {
		in.notifyRefreshResult(*s.negotiationRefresh)
	}
	if in.Request.URL.IsAbs() || in.Request.URL.Path != "/Items/item-1/PlaybackInfo" {
		s.t.Fatalf("negotiation URL = %q", in.Request.URL.String())
	}
	var request embyPlaybackInfoRequestDTO
	if err := json.NewDecoder(in.Request.Body).Decode(&request); err != nil {
		s.t.Fatal(err)
	}
	if request.UserID != "synthetic-user" {
		s.t.Fatalf("negotiation UserId = %q", request.UserID)
	}
	if _, err := s.leases.Validate(in.Session.GatewayTokenHash, "play-1", "", time.Time{}); !errors.Is(err, ErrNotFound) {
		s.t.Fatalf("lease registered before fallback acceptance: %v", err)
	}
	refreshed := in.Snapshot
	refreshed.token = "refreshed-token"
	*in.SnapshotRef = refreshed
	body, _ := json.Marshal(embyPlaybackInfoResponseDTO{
		PlaySessionID: "play-1",
		MediaSources: []embyMediaSourceInfoDTO{{
			ID: "source-1", Name: "movie", Container: "mkv",
			DirectStreamURL:      "/Videos/item-1/original.mkv?MediaSourceId=source-1&PlaySessionId=play-1&api_key=backend-token",
			SupportsDirectStream: true,
			RequiredHTTPHeaders:  map[string]string{"X-Required": "yes", "Authorization": "forbidden"},
		}},
	})
	registration := newNegotiationLeaseRegistration(s.leases, in.Session.GatewayTokenHash, negotiationSelectorSet{PlaySessionIDs: []PlaySessionID{"play-1"}}, routeclass.OperationPlaybackInfo, in.Request.Context(), in.Snapshot, nil)
	return negotiationUpstreamResponse{
		Response:     &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))},
		Registration: registration,
	}, nil
}

func (s *downloadFallbackPortSpy) RoundTripMedia(in mediaUpstreamRequest) (*http.Response, error) {
	s.calls = append(s.calls, "media")
	if s.cancelPhase == "media" && s.cancel != nil {
		s.cancel()
	}
	if s.mediaRefresh != nil {
		in.notifyRefreshResult(*s.mediaRefresh)
	}
	if in.Request.URL.IsAbs() || in.Request.URL.Path != "/Videos/item-1/original.mkv" {
		s.t.Fatalf("media URL = %q", in.Request.URL.String())
	}
	if in.Snapshot.token != "refreshed-token" {
		s.t.Fatalf("media snapshot token = %q", in.Snapshot.token)
	}
	if in.Request.Header.Get("Range") != "bytes=0-" || in.Request.Header.Get("X-Required") != "yes" || in.Request.Header.Get("Authorization") != "" || in.Request.Header.Get("X-Unrelated") != "" {
		s.t.Fatalf("media headers = %#v", in.Request.Header)
	}
	if _, err := s.leases.Validate(in.Session.GatewayTokenHash, "play-1", "", time.Time{}); err != nil {
		s.t.Fatalf("lease not committed immediately before media: %v", err)
	}
	if s.mediaErr != nil {
		return nil, s.mediaErr
	}
	body := &countingReadCloser{Reader: bytes.NewReader([]byte("data")), closes: &s.mediaBodyClose}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
}

func TestDownloadFallbackReleasesNegotiationLeaseWhenMediaFails(t *testing.T) {
	leases := NewMediaLeaseRegistry(func() time.Time { return time.Unix(100, 0) })
	spy := &downloadFallbackPortSpy{t: t, leases: leases, mediaErr: errors.New("media failed")}
	server := &Server{cfg: Config{GatewayBasePath: "/emby", GatewayServerID: "gateway"}, mediaUpstream: spy, mediaLeases: leases}
	session := &Session{GatewayTokenHash: "owner", SyntheticUserID: "synthetic-user"}
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item-1/Download?MediaSourceId=source-1&api_key=gateway-token", nil)
	request.Header.Set("Range", "bytes=0-")

	response, err := server.tryDownloadDirectStreamFallback(request, "/Items/item-1/Download", session, upstreamRequestSnapshot{baseURL: "http://backend.test/emby", userID: "backend-user", token: "backend-token", identity: BackendClientIdentity{DeviceID: "device"}}, "gateway-token")
	if !errors.Is(err, errDownloadFallbackUnavailable) || response != nil {
		t.Fatalf("response/error = %#v / %v", response, err)
	}
	if _, err := leases.Validate("owner", "play-1", "", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lease after failure = %v", err)
	}
}

type countingReadCloser struct {
	io.Reader
	closes *atomic.Int32
}

func (c *countingReadCloser) Close() error {
	c.closes.Add(1)
	return nil
}

func TestDownloadFallbackUsesPurposePortsAndReleasesLeaseOnCloseOnce(t *testing.T) {
	leases := NewMediaLeaseRegistry(func() time.Time { return time.Unix(100, 0) })
	spy := &downloadFallbackPortSpy{t: t, leases: leases, calls: []string{"media(download)"}}
	server := &Server{cfg: Config{GatewayBasePath: "/emby", GatewayServerID: "gateway"}, mediaUpstream: spy, mediaLeases: leases}
	session := &Session{GatewayTokenHash: "owner", SyntheticUserID: "synthetic-user"}
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item-1/Download?MediaSourceId=source-1&api_key=gateway-token", nil)
	request.Header.Set("Range", "bytes=0-")
	request.Header.Set("X-Unrelated", "drop")

	response, err := server.tryDownloadDirectStreamFallback(request, "/Items/item-1/Download", session, upstreamRequestSnapshot{baseURL: "http://backend.test/emby", userID: "backend-user", token: "backend-token", identity: BackendClientIdentity{DeviceID: "device"}}, "gateway-token")
	if err != nil {
		t.Fatal(err)
	}
	if len(spy.calls) != 3 || spy.calls[0] != "media(download)" || spy.calls[1] != "negotiation" || spy.calls[2] != "media" {
		t.Fatalf("port calls = %#v", spy.calls)
	}
	if _, err := leases.Validate("owner", "play-1", "", time.Time{}); err != nil {
		t.Fatalf("lease before close: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if spy.mediaBodyClose.Load() != 1 {
		t.Fatalf("media body closes = %d", spy.mediaBodyClose.Load())
	}
	if _, err := leases.Validate("owner", "play-1", "", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lease after close = %v", err)
	}
}

func TestDownloadFallbackPortsReportRefreshWithOriginatingRequest(t *testing.T) {
	for _, phase := range []string{"negotiation", "media"} {
		t.Run(phase, func(t *testing.T) {
			leases := NewMediaLeaseRegistry(func() time.Time { return time.Unix(100, 0) })
			result := upstreamRefreshResult{Confirmed: true, Err: errors.New("refresh failed")}
			spy := &downloadFallbackPortSpy{t: t, leases: leases}
			if phase == "negotiation" {
				spy.negotiationRefresh = &result
			} else {
				spy.mediaRefresh = &result
			}
			store := NewMemoryStore()
			em := observe.NewEmitter(8)
			defer em.Close()
			registry := telemetry.New(em)
			registryCtx, stopRegistry := context.WithCancel(context.Background())
			defer stopRegistry()
			go registry.Start(registryCtx)
			server := &Server{cfg: Config{GatewayBasePath: "/emby", GatewayServerID: "gateway"}, store: store, emitter: em, mediaUpstream: spy, mediaLeases: leases, leaseCleanupStop: make(chan struct{}), leaseCleanupDone: make(chan struct{})}
			session := &Session{GatewayTokenHash: "owner", GatewayUserID: "user-1", GatewayUsername: "alice", SyntheticUserID: "synthetic-user", Device: "phone"}
			request := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item-1/Download?MediaSourceId=source-1&api_key=gateway-token", nil)
			request.Header.Set("Range", "bytes=0-")

			response, err := server.tryDownloadDirectStreamFallback(request, "/Items/item-1/Download", session, upstreamRequestSnapshot{baseURL: "http://backend.test/emby", userID: "backend-user", token: "backend-token", identity: BackendClientIdentity{DeviceID: "device"}}, "gateway-token")
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if len(store.AuditLogs) != 1 {
				t.Fatalf("refresh audits = %#v", store.AuditLogs)
			}
			audit := store.AuditLogs[0]
			if audit.Event != "backend_token_refresh_failure" || audit.Method != http.MethodGet || audit.Path != "/Items/item-1/Download" || audit.RemoteIP != "192.0.2.1" || audit.GatewayUserID != session.GatewayUserID || audit.SyntheticUserID != session.SyntheticUserID {
				t.Fatalf("refresh audit attribution = %#v", audit)
			}
			status := waitForAuthState(t, registry, telemetry.AuthStateFailing)
			if status.LastAuthError != telemetry.AuthErrorRefreshFailed {
				t.Fatalf("fallback refresh health = %#v", status)
			}
		})
	}
}

func TestDownloadFallbackPortsSuppressCanceledRefresh(t *testing.T) {
	for _, phase := range []string{"negotiation", "media"} {
		t.Run(phase, func(t *testing.T) {
			leases := NewMediaLeaseRegistry(func() time.Time { return time.Unix(100, 0) })
			result := upstreamRefreshResult{Confirmed: true, Err: context.Canceled}
			ctx, cancel := context.WithCancel(context.Background())
			spy := &downloadFallbackPortSpy{t: t, leases: leases, cancelPhase: phase, cancel: cancel}
			if phase == "negotiation" {
				spy.negotiationRefresh = &result
			} else {
				spy.mediaRefresh = &result
			}
			store := NewMemoryStore()
			em := observe.NewEmitter(8)
			defer em.Close()
			registry := telemetry.New(em)
			registryCtx, stopRegistry := context.WithCancel(context.Background())
			defer stopRegistry()
			go registry.Start(registryCtx)
			server := &Server{cfg: Config{GatewayBasePath: "/emby", GatewayServerID: "gateway"}, store: store, emitter: em, mediaUpstream: spy, mediaLeases: leases, leaseCleanupStop: make(chan struct{}), leaseCleanupDone: make(chan struct{})}
			session := &Session{GatewayTokenHash: "owner", GatewayUserID: "user-1", SyntheticUserID: "synthetic-user"}
			server.emitBackendAuthRefresh(session, "backend_token_refresh", http.StatusOK)
			waitForAuthState(t, registry, telemetry.AuthStateHealthy)
			request := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item-1/Download?MediaSourceId=source-1&api_key=gateway-token", nil).WithContext(ctx)
			request.Header.Set("Range", "bytes=0-")

			response, err := server.tryDownloadDirectStreamFallback(request, "/Items/item-1/Download", session, upstreamRequestSnapshot{baseURL: "http://backend.test/emby", userID: "backend-user", token: "backend-token", identity: BackendClientIdentity{DeviceID: "device"}}, "gateway-token")
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if len(store.AuditLogs) != 0 {
				t.Fatalf("canceled refresh audits = %#v", store.AuditLogs)
			}
			status := waitForAuthState(t, registry, telemetry.AuthStateHealthy)
			if status.LastAuthError != "" {
				t.Fatalf("canceled fallback refresh changed auth health = %#v", status)
			}
		})
	}
}

type malformedDownloadFallbackPort struct {
	t      *testing.T
	leases MediaLeaseRegistry
	calls  int
}

func (p *malformedDownloadFallbackPort) RoundTripMedia(in mediaUpstreamRequest) (*http.Response, error) {
	if in.Request.URL.Path != "/Items/item-1/Download" {
		p.t.Fatalf("unexpected media request %q", in.Request.URL.Path)
	}
	return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString("original forbidden")), Request: in.Request}, nil
}

func (p *malformedDownloadFallbackPort) RoundTripNegotiation(in negotiationUpstreamRequest) (negotiationUpstreamResponse, error) {
	p.calls++
	playSessionID := PlaySessionID(fmt.Sprintf("malformed-%d", p.calls))
	body := fmt.Sprintf(`{"PlaySessionId":%q,"MediaSources":"malformed"}`, playSessionID)
	registration := newNegotiationLeaseRegistration(p.leases, in.Session.GatewayTokenHash, negotiationSelectorSet{PlaySessionIDs: []PlaySessionID{playSessionID}}, routeclass.OperationPlaybackInfo, in.Request.Context(), in.Snapshot, nil)
	return negotiationUpstreamResponse{
		Response:     &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(body))},
		Registration: registration,
	}, nil
}

func TestDownloadFallbackMalformedTypedNegotiationReleasesRegisteredLeases(t *testing.T) {
	store := testStore("http://backend.test/emby")
	session := testSession()
	store.Sessions[HashToken("gateway-token")] = session
	server := NewServer(Config{GatewayBasePath: "/emby", GatewayServerID: "gateway"}, store)
	port := &malformedDownloadFallbackPort{t: t, leases: server.mediaLeases}
	server.mediaUpstream = port

	for attempt := 0; attempt < mediaLeaseRegistryMaxPerToken+2; attempt++ {
		request := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item-1/Download?api_key=gateway-token", nil)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden || response.Body.String() != "original forbidden" {
			t.Fatalf("attempt %d response = %d %q", attempt, response.Code, response.Body.String())
		}
		if owners := server.mediaLeases.Owners(); len(owners) != 0 {
			t.Fatalf("attempt %d retained lease owners = %#v", attempt, owners)
		}
		if server.leaseCleanupStarted.Load() {
			t.Fatalf("attempt %d started cleanup maintenance", attempt)
		}
	}
	if port.calls != mediaLeaseRegistryMaxPerToken+2 {
		t.Fatalf("negotiation calls = %d", port.calls)
	}
}

type failingRegisterLeaseRegistry struct {
	MediaLeaseRegistry
}

func (failingRegisterLeaseRegistry) RegisterAll(string, []PlaySessionID, []LiveStreamID) error {
	return ErrStoreUnavailable
}

type failedDownloadFallbackPort struct {
	leases      MediaLeaseRegistry
	mediaCalls  int
	mediaFailed bool
}

func (p *failedDownloadFallbackPort) RoundTripMedia(in mediaUpstreamRequest) (*http.Response, error) {
	p.mediaCalls++
	if p.mediaCalls == 1 {
		return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString("original forbidden")), Request: in.Request}, nil
	}
	if p.mediaFailed {
		return nil, errors.New("fallback media failed")
	}
	return nil, errors.New("fallback media should not run")
}

func (p *failedDownloadFallbackPort) RoundTripNegotiation(in negotiationUpstreamRequest) (negotiationUpstreamResponse, error) {
	body, _ := json.Marshal(embyPlaybackInfoResponseDTO{
		PlaySessionID: "play-failed",
		MediaSources: []embyMediaSourceInfoDTO{{
			ID:                   "source-1",
			DirectStreamURL:      "/Videos/item-1/original.mkv?MediaSourceId=source-1&PlaySessionId=play-failed",
			SupportsDirectStream: true,
		}},
	})
	registration := newNegotiationLeaseRegistration(p.leases, in.Session.GatewayTokenHash, negotiationSelectorSet{PlaySessionIDs: []PlaySessionID{"play-failed"}}, routeclass.OperationPlaybackInfo, in.Request.Context(), in.Snapshot, nil)
	return negotiationUpstreamResponse{
		Response:     &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: in.Request},
		Registration: registration,
	}, nil
}

func TestDownloadFallbackCommitAndMediaFailurePreserveOriginalForbidden(t *testing.T) {
	for _, tt := range []struct {
		name        string
		mediaFailed bool
		failCommit  bool
		wantCalls   int
	}{
		{name: "commit failure", failCommit: true, wantCalls: 1},
		{name: "media failure", mediaFailed: true, wantCalls: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := testStore("http://backend.test/emby")
			store.Sessions[HashToken("gateway-token")] = testSession()
			server := NewServer(Config{GatewayBasePath: "/emby", GatewayServerID: "gateway"}, store)
			leases := NewMediaLeaseRegistry(nil)
			if tt.failCommit {
				server.mediaLeases = failingRegisterLeaseRegistry{MediaLeaseRegistry: leases}
			} else {
				server.mediaLeases = leases
			}
			port := &failedDownloadFallbackPort{leases: server.mediaLeases, mediaFailed: tt.mediaFailed}
			server.mediaUpstream = port

			request := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item-1/Download?MediaSourceId=source-1&api_key=gateway-token", nil)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)

			if response.Code != http.StatusForbidden || response.Body.String() != "original forbidden" {
				t.Fatalf("response = %d %q", response.Code, response.Body.String())
			}
			if port.mediaCalls != tt.wantCalls {
				t.Fatalf("media calls = %d, want %d", port.mediaCalls, tt.wantCalls)
			}
			if owners := leases.Owners(); len(owners) != 0 {
				t.Fatalf("failure retained owners = %#v", owners)
			}
		})
	}
}
