package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
)

func phase8DispatchServer(t *testing.T) (*Server, *MemoryStore, *phase5AuthSpy, *phase5MetadataSpy, *phase5MediaSpy) {
	t.Helper()
	return phase5DispatchServer(t)
}

func assertExactRouteNotFound(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "not found\n" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "not found\n")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") || !strings.Contains(ct, "charset=utf-8") {
		t.Fatalf("Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	if allow := rec.Header().Get("Allow"); allow != "" {
		t.Fatalf("Allow = %q, want empty", allow)
	}
}

func TestPhase8UnclassifiedAuthenticatedExact404(t *testing.T) {
	paths := []string{
		"/Unknown",
		"/Users/gateway-user/Suggestions",
		"/Items/item",
		"/Items/item/Images",
		"/ScheduledTasks",
		// Generic filename is Unclassified; exact /Videos/{id}/stream is admitted MediaProxy.
		"/Videos/item/original.mkv",
		"/Plugins",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			server, store, auth, metadata, media := phase8DispatchServer(t)
			defer server.Close()
			em := observe.NewEmitter(64)
			server.emitter = em
			store.AuditLogs = nil
			_ = drainEmitter(em)

			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby"+path+"?api_key=gateway-token", nil))
			assertExactRouteNotFound(t, rec)

			if auth.ensure != 0 || metadata.calls != 0 || media.media != 0 || media.negotiation != 0 {
				t.Fatalf("Ensure/adapters must stay zero: ensure=%d meta=%d media=%d neg=%d",
					auth.ensure, metadata.calls, media.media, media.negotiation)
			}
			if !hasAuditEvent(store, "route_not_found") {
				t.Fatalf("missing route_not_found audit: %#v", store.AuditLogs)
			}
			for _, entry := range store.AuditLogs {
				if entry.Event != "route_not_found" {
					continue
				}
				if entry.Status != http.StatusNotFound || entry.Method != http.MethodGet || entry.Path != path {
					t.Fatalf("audit fields incorrect: %#v", entry)
				}
				if entry.GatewayUserID != "u1" || entry.SyntheticUserID != "gateway-user" {
					t.Fatalf("audit identity incorrect: %#v", entry)
				}
				if strings.Contains(entry.Path, "api_key") || strings.Contains(entry.Message, "gateway-token") {
					t.Fatalf("audit leaked credentials: %#v", entry)
				}
			}
			events := drainEmitter(em)
			var denied int
			for _, ev := range events {
				if ev.Kind == observe.KindRequest && ev.Outcome == observe.OutcomeDenied {
					denied++
					if ev.RouteClass != observe.RouteOther || ev.StatusClass != observe.Status4xx {
						t.Fatalf("telemetry = %#v", ev)
					}
					if ev.UserID != "u1" || ev.SessionID != HashToken("gateway-token") {
						t.Fatalf("telemetry identity = %#v", ev)
					}
				}
				if ev.Kind == observe.KindRequest && ev.Outcome == observe.OutcomeOK {
					t.Fatalf("must not noteSession OK for Unclassified: %#v", ev)
				}
			}
			if denied != 1 {
				t.Fatalf("denied request events = %d, want 1; events=%#v", denied, events)
			}
		})
	}
}

func TestPhase8UnclassifiedUnknownMethodIs404NoAllow(t *testing.T) {
	server, store, auth, metadata, media := phase8DispatchServer(t)
	defer server.Close()
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "http://gateway.test/emby/Unknown?api_key=gateway-token", nil))
	assertExactRouteNotFound(t, rec)
	if auth.ensure != 0 || metadata.calls != 0 || media.media != 0 || media.negotiation != 0 {
		t.Fatalf("adapters must stay zero")
	}
	if !hasAuditEvent(store, "route_not_found") {
		t.Fatalf("missing route_not_found")
	}
}

func TestPhase8UnclassifiedUnauthenticatedIs401(t *testing.T) {
	server, store, auth, _, _ := phase8DispatchServer(t)
	defer server.Close()
	store.Sessions = map[string]*Session{}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Unknown", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if auth.ensure != 0 {
		t.Fatalf("Ensure = %d", auth.ensure)
	}
	if hasAuditEvent(store, "route_not_found") {
		t.Fatalf("unauthenticated must not emit route_not_found: %#v", store.AuditLogs)
	}
}

func TestPhase8UnclassifiedCredentialConflictIs400(t *testing.T) {
	// guardProxyQueryCredentials only conflicts on generic query key "token" when it
	// carries a different gateway-shaped active session token than the selected header.
	selected, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}
	other, _, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken: %v", err)
	}

	server, store, auth, metadata, media := phase8DispatchServer(t)
	defer server.Close()
	sess := testSession()
	sess.GatewayTokenHash = HashToken(selected)
	store.Sessions = map[string]*Session{sess.GatewayTokenHash: sess}
	otherSess := testSession()
	otherSess.GatewayTokenHash = HashToken(other)
	store.Sessions[otherSess.GatewayTokenHash] = otherSess
	store.AuditLogs = nil

	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Unknown?token="+other, nil)
	req.Header.Set("X-Emby-Token", selected)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	if auth.ensure != 0 || metadata.calls != 0 || media.media != 0 || media.negotiation != 0 {
		t.Fatalf("Ensure/adapters must stay zero")
	}
	if hasAuditEvent(store, "route_not_found") {
		t.Fatalf("credential conflict must not emit route_not_found")
	}
}

func TestPhase8ForeignUserPathBeforeUnclassified404(t *testing.T) {
	server, store, auth, metadata, media := phase8DispatchServer(t)
	defer server.Close()
	// Unclassified foreign path must 403 (not 404) with zero Ensure.
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Users/foreign/unknown?api_key=gateway-token", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%q", rec.Code, rec.Body.String())
	}
	if auth.ensure != 0 || metadata.calls != 0 || media.media != 0 || media.negotiation != 0 {
		t.Fatalf("Ensure/adapters must stay zero")
	}
	if hasAuditEvent(store, "route_not_found") {
		t.Fatalf("foreign path must not use route_not_found")
	}
	if !hasAuditEvent(store, "upstream_request_denied") {
		t.Fatalf("missing ownership denial audit: %#v", store.AuditLogs)
	}
}

func TestPhase8ForeignUserQueryBeforeEnsure(t *testing.T) {
	server, _, auth, metadata, media := phase8DispatchServer(t)
	defer server.Close()
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/System/Info?api_key=gateway-token&UserId=foreign", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if auth.ensure != 0 || metadata.calls != 0 || media.media != 0 || media.negotiation != 0 {
		t.Fatalf("Ensure/adapters must stay zero")
	}
}

func TestPhase8ForeignMetadataPathUsesRelNotEmbyPrefix(t *testing.T) {
	// Regression: validate must use API-relative rel. Full /emby/Users/... would
	// previously make relUserMatches treat the first segment as non-Users and allow.
	server, _, auth, metadata, media := phase8DispatchServer(t)
	defer server.Close()
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Users/foreign/Items?api_key=gateway-token", nil))
	if rec.Code != http.StatusForbidden || auth.ensure != 0 || metadata.calls != 0 || media.media != 0 || media.negotiation != 0 {
		t.Fatalf("status=%d ensure=%d adapters=%d/%d/%d", rec.Code, auth.ensure, metadata.calls, media.media, media.negotiation)
	}
}

func TestPhase8WrongMethodKnownRouteKeeps405(t *testing.T) {
	server, _, auth, metadata, media := phase8DispatchServer(t)
	defer server.Close()
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://gateway.test/emby/System/Info?api_key=gateway-token", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body=%q", rec.Code, rec.Body.String())
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Fatalf("Allow = %q, want GET, HEAD", allow)
	}
	if auth.ensure != 0 || metadata.calls != 0 || media.media != 0 || media.negotiation != 0 {
		t.Fatalf("Ensure/adapters must stay zero on method denial")
	}
}

func TestPhase8CuratedMetadataStillDispatches(t *testing.T) {
	server, _, auth, metadata, media := phase8DispatchServer(t)
	defer server.Close()
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/System/Info?api_key=gateway-token", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if auth.ensure != 1 || metadata.calls != 1 || media.media != 0 || media.negotiation != 0 {
		t.Fatalf("ensure=%d meta=%d media=%d neg=%d", auth.ensure, metadata.calls, media.media, media.negotiation)
	}
}

func TestPhase8ValidateRequestUserOwnership(t *testing.T) {
	if err := validateRequestUserOwnership("/Users/gateway-user/Items", "", "gateway-user"); err != nil {
		t.Fatalf("own path: %v", err)
	}
	if err := validateRequestUserOwnership("/System/Info", "", "gateway-user"); err != nil {
		t.Fatalf("non-user path: %v", err)
	}
	if err := validateRequestUserOwnership("/Users/foreign/Items", "", "gateway-user"); err != ErrForbidden {
		t.Fatalf("foreign path err = %v", err)
	}
	if err := validateRequestUserOwnership("/System/Info", "UserId=foreign", "gateway-user"); err != ErrForbidden {
		t.Fatalf("foreign query err = %v", err)
	}
	if err := validateRequestUserOwnership("/Users/gateway-user/Items", "UserId=gateway-user", "gateway-user"); err != nil {
		t.Fatalf("own query: %v", err)
	}
	if got := observe.RouteClassOf(routeclass.Decision{Ownership: routeclass.Unclassified, Operation: routeclass.OperationUnclassified}); got != observe.RouteOther {
		t.Fatalf("RouteClassOf Unclassified = %q", got)
	}
}

func TestPhase8UnclassifiedDoesNotNoteSessionOK(t *testing.T) {
	server, _, auth, metadata, media := phase8DispatchServer(t)
	defer server.Close()
	em := observe.NewEmitter(64)
	server.emitter = em
	_ = drainEmitter(em)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Unknown?api_key=gateway-token", nil))
	assertExactRouteNotFound(t, rec)
	if auth.ensure != 0 || metadata.calls != 0 || media.media != 0 || media.negotiation != 0 {
		t.Fatal("zero-load invariant broken")
	}
	for _, ev := range drainEmitter(em) {
		if ev.Kind == observe.KindRequest && ev.Outcome == observe.OutcomeOK {
			t.Fatalf("activity/noteSession OK event: %#v", ev)
		}
	}
}

func TestPhase8UnclassifiedPanicTransportNeverDialed(t *testing.T) {
	server, _, auth, _, _ := phase8DispatchServer(t)
	defer server.Close()
	rec := httptest.NewRecorder()
	// phase5DispatchServer installs phase5PanicTransport on the HTTP client.
	// Must not panic: no dial / Ensure.
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Unknown?api_key=gateway-token", nil))
	assertExactRouteNotFound(t, rec)
	if auth.ensure != 0 {
		t.Fatalf("Ensure = %d", auth.ensure)
	}
}

func TestPhase8UnclassifiedGlobalItemsAndShowsBeforePersonal(t *testing.T) {
	// P1: classifier is authoritative; personal handlers must not run for Unclassified.
	paths := []string{
		"/Items?api_key=gateway-token",
		"/Items?api_key=gateway-token&IsFavorite=true",
		"/Items?api_key=gateway-token&Filters=IsPlayed",
		"/Shows/show-1/Episodes?api_key=gateway-token",
		"/Shows/show-1/Episodes?api_key=gateway-token&IsFavorite=true",
		"/Shows/show-1/Seasons?api_key=gateway-token",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			server, store, auth, metadata, media := phase8DispatchServer(t)
			defer server.Close()
			store.AuditLogs = nil
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby"+path, nil))
			assertExactRouteNotFound(t, rec)
			if auth.ensure != 0 || metadata.calls != 0 || media.media != 0 || media.negotiation != 0 {
				t.Fatalf("zero-egress broken: ensure=%d meta=%d media=%d neg=%d", auth.ensure, metadata.calls, media.media, media.negotiation)
			}
			if !hasAuditEvent(store, "route_not_found") {
				t.Fatalf("missing route_not_found: %#v", store.AuditLogs)
			}
		})
	}
}

func TestPhase8ClassifiedPersonalRoutesStillRun(t *testing.T) {
	// Regression: Users/{id}/Items, Resume, NextUp remain classifier-owned and handled.
	server, store, auth, metadata, media := phase8DispatchServer(t)
	defer server.Close()
	// Resume is LocalPersonal — local handler, no metadata Ensure required for empty resume.
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Users/gateway-user/Items/Resume?api_key=gateway-token", nil))
	if rec.Code == http.StatusNotFound && rec.Body.String() == "not found\n" {
		t.Fatalf("Resume must not be Unclassified 404")
	}
	// NextUp local personal
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Shows/NextUp?api_key=gateway-token", nil))
	if rec.Code == http.StatusNotFound && rec.Body.String() == "not found\n" {
		t.Fatalf("NextUp must not be Unclassified 404")
	}
	// Users Items is MetadataProxy — may Ensure once for empty list
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Users/gateway-user/Items?api_key=gateway-token", nil))
	if rec.Code == http.StatusNotFound && rec.Body.String() == "not found\n" {
		t.Fatalf("Users/Items must not be Unclassified 404")
	}
	_ = store
	_ = auth
	_ = metadata
	_ = media
}
