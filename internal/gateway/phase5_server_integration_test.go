package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type phase5PanicTransport struct{}

func (phase5PanicTransport) RoundTrip(*http.Request) (*http.Response, error) {
	panic("direct HTTP client bypass")
}

type phase5AuthSpy struct {
	runtime *UpstreamRuntime
	ensure  int
}

func (s *phase5AuthSpy) Ensure(context.Context) (*UpstreamRuntime, error) {
	s.ensure++
	return s.runtime, nil
}
func (s *phase5AuthSpy) Refresh(context.Context, string) (*UpstreamRuntime, error) {
	return s.runtime, nil
}
func (s *phase5AuthSpy) Probe(managedAuthProbeRequest) (UpstreamServerInfoUpdate, error) {
	return UpstreamServerInfoUpdate{}, nil
}
func (s *phase5AuthSpy) Login(managedAuthLoginRequest) (UpstreamAuthUpdate, error) {
	return UpstreamAuthUpdate{}, nil
}
func (s *phase5AuthSpy) Logout(managedAuthLogoutRequest) error { return nil }

type phase5MetadataSpy struct{ calls int }

func (s *phase5MetadataSpy) RoundTripMetadata(in metadataUpstreamRequest) (*http.Response, error) {
	s.calls++
	return phase5Response(in.Request, http.StatusOK, "application/json", `{}`), nil
}

type phase5MediaSpy struct {
	media       int
	negotiation int
}

type phase5FailMedia struct{ err error }

func (s phase5FailMedia) RoundTripMedia(mediaUpstreamRequest) (*http.Response, error) {
	return nil, s.err
}
func (s phase5FailMedia) RoundTripNegotiation(negotiationUpstreamRequest) (negotiationUpstreamResponse, error) {
	return negotiationUpstreamResponse{}, s.err
}

func (s *phase5MediaSpy) RoundTripMedia(in mediaUpstreamRequest) (*http.Response, error) {
	s.media++
	return phase5Response(in.Request, http.StatusOK, "video/mp4", "media"), nil
}
func (s *phase5MediaSpy) RoundTripNegotiation(in negotiationUpstreamRequest) (negotiationUpstreamResponse, error) {
	s.negotiation++
	return negotiationUpstreamResponse{Response: phase5Response(in.Request, http.StatusOK, "application/json", `{}`)}, nil
}

type phase5LegacySpy struct{ calls int }

func (s *phase5LegacySpy) RoundTripLegacy(in legacyUpstreamRequest) (*http.Response, error) {
	s.calls++
	return phase5Response(in.Request, http.StatusOK, "application/json", `{}`), nil
}

func phase5Response(request *http.Request, status int, contentType, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: request}
}

func phase5DispatchServer(t *testing.T) (*Server, *MemoryStore, *phase5AuthSpy, *phase5MetadataSpy, *phase5MediaSpy, *phase5LegacySpy) {
	t.Helper()
	store := testStore("http://backend.invalid/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	server := NewServer(Config{GatewayBasePath: "/emby", HTTPClient: &http.Client{Transport: phase5PanicTransport{}}}, store)
	runtime, err := store.LoadDefaultUpstreamRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	auth := &phase5AuthSpy{runtime: runtime}
	metadata := &phase5MetadataSpy{}
	media := &phase5MediaSpy{}
	legacy := &phase5LegacySpy{}
	server.managedAuthUpstream = auth
	server.metadataUpstream = metadata
	server.mediaUpstream = media
	server.legacyHTTPUpstream = legacy
	return server, store, auth, metadata, media, legacy
}

func TestPhase5ServerDispatchesOnlyToExactAdapter(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		want   string
	}{
		{"metadata", http.MethodGet, "/Items/item", "metadata"},
		{"media", http.MethodGet, "/Videos/item/stream", "media"},
		{"playback info get", http.MethodGet, "/Items/item/PlaybackInfo", "negotiation"},
		{"playback info post", http.MethodPost, "/Items/item/PlaybackInfo", "negotiation"},
		{"live open", http.MethodPost, "/LiveStreams/Open", "negotiation"},
		{"live info", http.MethodPost, "/LiveStreams/MediaInfo", "negotiation"},
		{"live close", http.MethodPost, "/LiveStreams/Close", "negotiation"},
		{"encoding delete", http.MethodDelete, "/Videos/ActiveEncodings", "negotiation"},
		{"encoding delete compat", http.MethodPost, "/Videos/ActiveEncodings/Delete", "negotiation"},
		{"legacy", http.MethodGet, "/Unknown", "legacy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, _, auth, metadata, media, legacy := phase5DispatchServer(t)
			defer server.Close()
			var body io.Reader
			if tt.method == http.MethodPost {
				body = strings.NewReader(`{}`)
			}
			req := httptest.NewRequest(tt.method, "http://gateway.test/emby"+tt.path+"?api_key=gateway-token", body)
			if body != nil {
				req.Header.Set("Content-Type", "application/json")
			}
			writer := httptest.NewRecorder()
			server.ServeHTTP(writer, req)
			if writer.Code != http.StatusOK {
				t.Fatalf("status = %d body=%q", writer.Code, writer.Body.String())
			}
			got := map[string]int{"metadata": metadata.calls, "media": media.media, "negotiation": media.negotiation, "legacy": legacy.calls}
			for name, count := range got {
				want := 0
				if name == tt.want {
					want = 1
				}
				if count != want {
					t.Fatalf("%s calls = %d, want %d; all=%v", name, count, want, got)
				}
			}
			if auth.ensure != 1 {
				t.Fatalf("Ensure calls = %d", auth.ensure)
			}
		})
	}
}

func TestPhase5MetadataForeignSelectorDeniedBeforeEnsure(t *testing.T) {
	server, _, auth, metadata, media, legacy := phase5DispatchServer(t)
	defer server.Close()
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Users/foreign/Items?api_key=gateway-token&UserId=foreign", nil)
	writer := httptest.NewRecorder()
	server.ServeHTTP(writer, req)
	if writer.Code != http.StatusForbidden || auth.ensure != 0 || metadata.calls != 0 || media.media != 0 || media.negotiation != 0 || legacy.calls != 0 {
		t.Fatalf("status=%d ensure=%d adapters=%d/%d/%d/%d", writer.Code, auth.ensure, metadata.calls, media.media, media.negotiation, legacy.calls)
	}
}

func TestPhase5LeaseLifecycleLogoutInactiveAndPeriodic(t *testing.T) {
	server, store, _, _, _, _ := phase5DispatchServer(t)
	defer server.Close()
	owner := HashToken("gateway-token")
	register := func(id string) {
		if err := server.mediaLeases.Register(MediaLease{GatewayTokenHash: owner, PlaySessionID: PlaySessionID(id)}); err != nil {
			t.Fatal(err)
		}
	}
	register("logout")
	logout := httptest.NewRequest(http.MethodPost, "http://gateway.test/emby/Sessions/Logout?api_key=gateway-token", nil)
	logoutWriter := httptest.NewRecorder()
	server.ServeHTTP(logoutWriter, logout)
	if logoutWriter.Code != http.StatusOK {
		t.Fatalf("logout status=%d", logoutWriter.Code)
	}
	if _, err := server.mediaLeases.Validate(owner, "logout", "", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("logout lease survived: %v", err)
	}

	store.Sessions[owner] = testSession()
	register("inactive")
	session := store.Sessions[owner]
	revokedAt := time.Now().UTC()
	session.RevokedAt = &revokedAt
	store.Sessions[owner] = session
	requestWriter := httptest.NewRecorder()
	server.ServeHTTP(requestWriter, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item?api_key=gateway-token", nil))
	if requestWriter.Code != http.StatusUnauthorized {
		t.Fatalf("inactive status=%d", requestWriter.Code)
	}
	if _, err := server.mediaLeases.Validate(owner, "inactive", "", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("inactive lease survived: %v", err)
	}

	store.Sessions[owner] = testSession()
	register("periodic")
	session = store.Sessions[owner]
	revokedAt = time.Now().UTC()
	session.RevokedAt = &revokedAt
	store.Sessions[owner] = session
	server.revalidateMediaLeaseOwners(context.Background(), time.Now().UTC())
	if _, err := server.mediaLeases.Validate(owner, "periodic", "", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("periodic lease survived: %v", err)
	}
}

func TestPhase5ServerAuditsRedirectDenial(t *testing.T) {
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHits++ }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/secret?api_key=backend-token", http.StatusFound)
	}))
	defer origin.Close()
	store := testStore(origin.URL)
	store.Sessions[HashToken("gateway-token")] = testSession()
	server := NewServer(Config{GatewayBasePath: "/emby", HTTPClient: origin.Client()}, store)
	defer server.Close()
	writer := httptest.NewRecorder()
	server.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Unknown?api_key=gateway-token", nil))
	if writer.Code != http.StatusBadGateway || targetHits != 0 || !hasAuditEvent(store, "upstream_redirect_denied") {
		t.Fatalf("status=%d targetHits=%d audits=%#v", writer.Code, targetHits, store.AuditLogs)
	}
	for _, entry := range store.AuditLogs {
		if entry.Event == "upstream_redirect_denied" && strings.Contains(entry.Path+entry.Message, "backend-token") {
			t.Fatalf("redirect audit leaked secret: %#v", entry)
		}
	}
}

func TestPhase5LeaseAndCapacityStatusMapping(t *testing.T) {
	server, _, auth, metadata, media, legacy := phase5DispatchServer(t)
	defer server.Close()
	if err := server.mediaLeases.Register(MediaLease{GatewayTokenHash: "foreign-owner", PlaySessionID: "foreign-play"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	server.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream?api_key=gateway-token&PlaySessionId=foreign-play", nil))
	if w.Code != http.StatusNotFound || auth.ensure != 0 || metadata.calls != 0 || media.media != 0 || media.negotiation != 0 || legacy.calls != 0 {
		t.Fatalf("foreign status=%d ensure=%d adapters=%d/%d/%d/%d", w.Code, auth.ensure, metadata.calls, media.media, media.negotiation, legacy.calls)
	}

	server, _, auth, _, _, _ = phase5DispatchServer(t)
	defer server.Close()
	server.mediaUpstream = phase5FailMedia{err: ErrStoreUnavailable}
	w = httptest.NewRecorder()
	server.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream?api_key=gateway-token", nil))
	if w.Code != http.StatusServiceUnavailable || auth.ensure != 1 {
		t.Fatalf("capacity status=%d ensure=%d", w.Code, auth.ensure)
	}
}

func TestPhase5NegotiationOversizeRejectedBeforeEnsure(t *testing.T) {
	server, _, auth, metadata, media, legacy := phase5DispatchServer(t)
	defer server.Close()
	body := strings.NewReader(strings.Repeat("x", negotiationRequestBodyLimit+1))
	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/emby/LiveStreams/Open?api_key=gateway-token", body)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge || auth.ensure != 0 || metadata.calls != 0 || media.media != 0 || media.negotiation != 0 || legacy.calls != 0 {
		t.Fatalf("status=%d ensure=%d adapters=%d/%d/%d/%d", w.Code, auth.ensure, metadata.calls, media.media, media.negotiation, legacy.calls)
	}
}

func TestPhase5MixedNegotiationIdentifiersRejectedBeforeEnsure(t *testing.T) {
	server, _, auth, metadata, media, legacy := phase5DispatchServer(t)
	defer server.Close()
	owner := HashToken("gateway-token")
	if err := server.mediaLeases.Register(MediaLease{GatewayTokenHash: owner, PlaySessionID: "owned"}); err != nil {
		t.Fatal(err)
	}
	if err := server.mediaLeases.Register(MediaLease{GatewayTokenHash: "foreign", LiveStreamID: "foreign"}); err != nil {
		t.Fatal(err)
	}
	body := strings.NewReader(`{"LiveStreamId":"foreign"}`)
	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/emby/LiveStreams/Close?api_key=gateway-token&PlaySessionId=owned", body)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound || auth.ensure != 0 || metadata.calls != 0 || media.media != 0 || media.negotiation != 0 || legacy.calls != 0 {
		t.Fatalf("status=%d ensure=%d adapters=%d/%d/%d/%d", w.Code, auth.ensure, metadata.calls, media.media, media.negotiation, legacy.calls)
	}
}

func TestPhase5LegacyOversizeMapsToRequestEntityTooLargeBeforeDial(t *testing.T) {
	server, _, auth, _, _, _ := phase5DispatchServer(t)
	defer server.Close()
	server.legacyHTTPUpstream = newLegacyHTTPUpstream(&http.Client{Transport: phase5PanicTransport{}}, nil, nil, nil)
	body := strings.NewReader(strings.Repeat("x", legacyRequestBodyLimit+1))
	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/emby/Unknown?api_key=gateway-token", body)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge || auth.ensure != 1 {
		t.Fatalf("status=%d ensure=%d", w.Code, auth.ensure)
	}
}

func TestServerCloseStopsLeaseMaintenanceIdempotently(t *testing.T) {
	server, _, _, _, _, _ := phase5DispatchServer(t)
	server.leaseCleanupInterval = time.Hour
	server.startMediaLeaseCleanup()
	server.Close()
	server.Close()
	select {
	case <-server.leaseCleanupDone:
	default:
		t.Fatal("lease cleanup goroutine still running")
	}
}
