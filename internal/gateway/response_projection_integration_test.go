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

	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
)

func TestResponseProjectionRoutePlan(t *testing.T) {
	tests := []struct {
		name, method, path string
		want               responseProjectionKind
	}{
		{"opaque image metadata", http.MethodGet, "/Items/item/Images", responseProjectionOpaque},
		{"opaque counts reserved", http.MethodGet, "/Items/Counts", responseProjectionOpaque},
		{"opaque prefixes reserved", http.MethodGet, "/Items/Prefixes", responseProjectionOpaque},
		{"opaque intros reserved", http.MethodGet, "/Items/Intros", responseProjectionOpaque},
		{"neighbor item detail", http.MethodGet, "/Items/Intro", responseProjectionBaseItem},
		{"opaque artist prefixes reserved", http.MethodGet, "/Artists/Prefixes", responseProjectionOpaque},
		{"neighbor artist by name", http.MethodGet, "/Artists/Prefix", responseProjectionBaseItem},
		{"opaque scheduled tasks", http.MethodGet, "/ScheduledTasks", responseProjectionOpaque},
		{"system info", http.MethodGet, "/System/Info", responseProjectionSystemInfo},
		{"direct item", http.MethodGet, "/Items/item", responseProjectionBaseItem},
		{"user direct item", http.MethodGet, "/Users/user/Items/item", responseProjectionBaseItem},
		{"user root item", http.MethodGet, "/Users/user/Items/Root", responseProjectionBaseItem},
		{"artist by name", http.MethodGet, "/Artists/name", responseProjectionBaseItem},
		{"genre by name", http.MethodGet, "/Genres/name", responseProjectionBaseItem},
		{"person by name", http.MethodGet, "/Persons/name", responseProjectionBaseItem},
		{"studio by name", http.MethodGet, "/Studios/name", responseProjectionBaseItem},
		{"items envelope", http.MethodGet, "/Items", responseProjectionBaseItemEnvelope},
		{"artists envelope", http.MethodGet, "/Artists", responseProjectionBaseItemEnvelope},
		{"album artists envelope", http.MethodGet, "/Artists/AlbumArtists", responseProjectionBaseItemEnvelope},
		{"artist instant mix", http.MethodGet, "/Artists/InstantMix", responseProjectionBaseItemEnvelope},
		{"genres envelope", http.MethodGet, "/Genres", responseProjectionBaseItemEnvelope},
		{"persons envelope", http.MethodGet, "/Persons", responseProjectionBaseItemEnvelope},
		{"studios envelope", http.MethodGet, "/Studios", responseProjectionBaseItemEnvelope},
		{"trailers envelope", http.MethodGet, "/Trailers", responseProjectionBaseItemEnvelope},
		{"channels envelope", http.MethodGet, "/Channels", responseProjectionBaseItemEnvelope},
		{"item similar", http.MethodGet, "/Items/item/Similar", responseProjectionBaseItemEnvelope},
		{"item instant mix", http.MethodGet, "/Items/item/InstantMix", responseProjectionBaseItemEnvelope},
		{"critic reviews", http.MethodGet, "/Items/item/CriticReviews", responseProjectionBaseItemEnvelope},
		{"movie similar", http.MethodGet, "/Movies/item/Similar", responseProjectionBaseItemEnvelope},
		{"user items", http.MethodGet, "/Users/user/Items", responseProjectionBaseItemEnvelope},
		{"user views", http.MethodGet, "/Users/user/Views", responseProjectionBaseItemEnvelope},
		{"user suggestions", http.MethodGet, "/Users/user/Suggestions", responseProjectionBaseItemEnvelope},
		{"section items", http.MethodGet, "/Users/user/Sections/section/Items", responseProjectionBaseItemEnvelope},
		{"intros", http.MethodGet, "/Users/user/Items/item/Intros", responseProjectionBaseItemEnvelope},
		{"show episodes", http.MethodGet, "/Shows/show/Episodes", responseProjectionBaseItemEnvelope},
		{"show seasons", http.MethodGet, "/Shows/show/Seasons", responseProjectionBaseItemEnvelope},
		{"missing shows", http.MethodGet, "/Shows/Missing", responseProjectionBaseItemEnvelope},
		{"upcoming shows", http.MethodGet, "/Shows/Upcoming", responseProjectionBaseItemEnvelope},
		{"next up envelope", http.MethodGet, "/Shows/NextUp", responseProjectionBaseItemEnvelope},
		{"resume envelope", http.MethodGet, "/Users/user/Items/Resume", responseProjectionBaseItemEnvelope},
		{"additional parts", http.MethodGet, "/Videos/item/AdditionalParts", responseProjectionBaseItemEnvelope},
		{"theme songs", http.MethodGet, "/Items/item/ThemeSongs", responseProjectionBaseItemEnvelope},
		{"theme videos", http.MethodGet, "/Items/item/ThemeVideos", responseProjectionBaseItemEnvelope},
		{"ancestors array", http.MethodGet, "/Items/item/Ancestors", responseProjectionBaseItemArray},
		{"declared latest array", http.MethodGet, "/Users/user/Items/Latest", responseProjectionBaseItemArray},
		{"local trailers array", http.MethodGet, "/Users/user/Items/item/LocalTrailers", responseProjectionBaseItemArray},
		{"special features array", http.MethodGet, "/Users/user/Items/item/SpecialFeatures", responseProjectionBaseItemArray},
		{"recommendations", http.MethodGet, "/Movies/Recommendations", responseProjectionBaseItemEnvelopeArray},
		{"all theme media", http.MethodGet, "/Items/item/ThemeMedia", responseProjectionAllThemeMedia},
		{"playback info", http.MethodPost, "/Items/item/PlaybackInfo", responseProjectionPlaybackInfo},
		{"live stream response", http.MethodPost, "/LiveStreams/Open", responseProjectionLiveStreamResponse},
		{"live stream media info", http.MethodPost, "/LiveStreams/MediaInfo", responseProjectionMediaSource},
		// Phase 8: unknown routes are Unclassified → opaque projection (not Legacy).
		{"unclassified unknown", http.MethodPost, "/Plugin/Unknown", responseProjectionOpaque},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := routeclass.Classify(test.method, test.path)
			if got := responseProjectionForRoute(test.method, test.path, decision).kind; got != test.want {
				t.Fatalf("projection=%v, want %v; decision=%+v", got, test.want, decision)
			}
		})
	}
}

func TestStrictProjectionRejectsNonJSONContentType(t *testing.T) {
	server, session := responseProjectionTestServer(t)
	for _, test := range []struct {
		name, method, path string
	}{
		{name: "base item", method: http.MethodGet, path: "/Items/item"},
		{name: "playback info", method: http.MethodPost, path: "/Items/item/PlaybackInfo"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := writeProjectedTestResponse(server, session, test.method, test.path, http.StatusOK, "video/mp4", "media", http.Header{"ETag": {`"unsafe"`}})
			if response.Code != http.StatusBadGateway || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("ETag") != "" || response.Body.String() != "backend unavailable\n" {
				t.Fatalf("status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
			}
		})
	}
}

func TestResponseHeaderPlanRejectsCredentialEncodingsAndPreservesGatewayPolicy(t *testing.T) {
	server, session := responseProjectionTestServer(t)
	for _, unsafe := range []string{"prefix-backend-token-suffix", `prefix-backend\u002dtoken-suffix`, "prefix-backend%2Dtoken-suffix"} {
		request := httptest.NewRequest(http.MethodGet, "https://gateway.test/emby/Items/item", nil)
		response := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}, "X-Custom": {unsafe}, "Vary": {"Accept-Encoding"}},
			Body:       io.NopCloser(strings.NewReader(`{"Id":`)), Request: request,
		}
		writer := httptest.NewRecorder()
		writer.Header().Set("Access-Control-Allow-Origin", "https://client.test")
		writer.Header().Set("Vary", "Origin")
		server.writeProxyResponseWithProjection(writer, request, "/Items/item", response, session, upstreamRequestSnapshot{token: "backend-token"}, "gateway-token", "https://gateway.test/emby", newResponseProjection(responseProjectionBaseItem))
		if writer.Code != http.StatusBadGateway || writer.Header().Get("X-Custom") != "" || writer.Header().Get("Access-Control-Allow-Origin") != "https://client.test" || writer.Header().Get("Vary") != "Origin" || writer.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unsafe=%q status=%d headers=%v", unsafe, writer.Code, writer.Header())
		}
	}

	request := httptest.NewRequest(http.MethodGet, "https://gateway.test/emby/Videos/item/stream", nil)
	request = request.WithContext(context.WithValue(request.Context(), resourceCookieContextKey{}, resourceRouteMedia))
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"video/mp4"}, "Cache-Control": {"public, max-age=60"}, "Vary": {"Accept-Encoding"}}, Body: io.NopCloser(strings.NewReader("media")), ContentLength: 5, Request: request}
	writer := httptest.NewRecorder()
	writer.Header().Set("Access-Control-Allow-Origin", "https://client.test")
	writer.Header().Set("Vary", "Origin")
	server.writeProxyResponseWithProjection(writer, request, "/Videos/item/stream", response, session, upstreamRequestSnapshot{}, "", "", newResponseProjection(responseProjectionOpaque))
	if writer.Header().Get("Access-Control-Allow-Origin") != "https://client.test" || writer.Header().Get("Vary") != "Origin, Accept-Encoding, Cookie" || writer.Header().Get("Cache-Control") != "private" {
		t.Fatalf("headers=%v", writer.Header())
	}
}

func TestOpaqueJSONResponsePreservesExactEntity(t *testing.T) {
	server, session := responseProjectionTestServer(t)
	body := " {\n \"Unknown\":1, \"Unknown\":2\n}\n"
	headers := make(http.Header)
	headers.Set("ETag", `"opaque"`)
	headers.Set("Last-Modified", "yesterday")
	headers.Set("Content-MD5", "digest")
	contentType := `application/vnd.emby.plugin+json; profile="future"`
	response := writeProjectedTestResponse(server, session, http.MethodGet, "/Items/item/Images", http.StatusOK, contentType, body, headers)
	if response.Code != http.StatusOK || response.Body.String() != body || response.Header().Get("Content-Type") != contentType || response.Header().Get("ETag") != `"opaque"` || response.Header().Get("Last-Modified") != "yesterday" || response.Header().Get("Content-MD5") != "digest" {
		t.Fatalf("status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
}

func TestNegotiationRegistrationGate(t *testing.T) {
	server, session := responseProjectionTestServer(t)
	registry := NewMediaLeaseRegistry(nil)
	selectors := negotiationSelectorSet{PlaySessionIDs: []PlaySessionID{"play"}, LiveStreamIDs: []LiveStreamID{"live"}}
	t.Run("success commits before headers", func(t *testing.T) {
		registration := newNegotiationLeaseRegistration(registry, "owner", selectors, routeclass.OperationLiveStreamOpen, context.Background(), upstreamRequestSnapshot{}, nil)
		request := httptest.NewRequest(http.MethodPost, "https://gateway.test/emby/LiveStreams/Open", nil)
		response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"MediaSource":{"ServerId":"backend-server"}}`)), Request: request}
		writer := &leaseGateWriter{ResponseRecorder: httptest.NewRecorder(), registry: registry, owner: "owner", play: "play", live: "live"}
		server.writeProxyResponseWithProjectionGate(writer, request, "/LiveStreams/Open", response, session, upstreamRequestSnapshot{serverID: "backend-server"}, "", "", newResponseProjection(responseProjectionLiveStreamResponse), registration)
		if writer.Code != http.StatusOK || !writer.committedBeforeHeader {
			t.Fatalf("status=%d committed=%v body=%q", writer.Code, writer.committedBeforeHeader, writer.Body.String())
		}
	})
	t.Run("projection failure rolls back once", func(t *testing.T) {
		closed := 0
		registration := newNegotiationLeaseRegistration(registry, "owner-2", negotiationSelectorSet{LiveStreamIDs: []LiveStreamID{"live-2"}}, routeclass.OperationLiveStreamOpen, context.Background(), upstreamRequestSnapshot{}, func(context.Context, upstreamRequestSnapshot, PlaySessionID, LiveStreamID) error {
			closed++
			return nil
		})
		request := httptest.NewRequest(http.MethodPost, "https://gateway.test/emby/LiveStreams/Open", nil)
		response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"MediaSource":`)), Request: request}
		writer := httptest.NewRecorder()
		server.writeProxyResponseWithProjectionGate(writer, request, "/LiveStreams/Open", response, session, upstreamRequestSnapshot{}, "", "", newResponseProjection(responseProjectionLiveStreamResponse), registration)
		registration.Close()
		if writer.Code != http.StatusBadGateway || closed != 1 {
			t.Fatalf("status=%d closed=%d", writer.Code, closed)
		}
		if _, err := registry.Validate("owner-2", "", "live-2", time.Time{}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("lease survived rollback: %v", err)
		}
	})
	t.Run("strict MIME failure rolls back once", func(t *testing.T) {
		strictRegistry := NewMediaLeaseRegistry(nil)
		closed := 0
		registration := newNegotiationLeaseRegistration(strictRegistry, "mime-owner", negotiationSelectorSet{LiveStreamIDs: []LiveStreamID{"mime-live"}}, routeclass.OperationLiveStreamOpen, context.Background(), upstreamRequestSnapshot{}, func(context.Context, upstreamRequestSnapshot, PlaySessionID, LiveStreamID) error {
			closed++
			return nil
		})
		request := httptest.NewRequest(http.MethodPost, "https://gateway.test/emby/LiveStreams/Open", nil)
		response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"video/mp4"}}, Body: io.NopCloser(strings.NewReader("media")), Request: request}
		writer := httptest.NewRecorder()
		server.writeProxyResponseWithProjectionGate(writer, request, "/LiveStreams/Open", response, session, upstreamRequestSnapshot{}, "", "", newResponseProjection(responseProjectionLiveStreamResponse), registration)
		if writer.Code != http.StatusBadGateway || writer.Header().Get("Cache-Control") != "no-store" || closed != 1 || len(strictRegistry.Owners()) != 0 {
			t.Fatalf("status=%d headers=%v closed=%d owners=%v", writer.Code, writer.Header(), closed, strictRegistry.Owners())
		}
	})
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{name: "not found", err: ErrNotFound, status: http.StatusNotFound},
		{name: "store unavailable", err: ErrStoreUnavailable, status: http.StatusServiceUnavailable},
	} {
		t.Run("commit "+test.name, func(t *testing.T) {
			closed := 0
			failing := &failingLeaseRegistry{MediaLeaseRegistry: NewMediaLeaseRegistry(nil), err: test.err}
			registration := newNegotiationLeaseRegistration(failing, "owner", negotiationSelectorSet{LiveStreamIDs: []LiveStreamID{"live"}}, routeclass.OperationLiveStreamOpen, context.Background(), upstreamRequestSnapshot{}, func(context.Context, upstreamRequestSnapshot, PlaySessionID, LiveStreamID) error {
				closed++
				return nil
			})
			request := httptest.NewRequest(http.MethodPost, "https://gateway.test/emby/LiveStreams/Open", nil)
			response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"MediaSource":null}`)), Request: request}
			writer := httptest.NewRecorder()
			server.writeProxyResponseWithProjectionGate(writer, request, "/LiveStreams/Open", response, session, upstreamRequestSnapshot{}, "", "", newResponseProjection(responseProjectionLiveStreamResponse), registration)
			if writer.Code != test.status || closed != 1 || writer.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d closed=%d headers=%v", writer.Code, closed, writer.Header())
			}
		})
	}
}

type leaseGateWriter struct {
	*httptest.ResponseRecorder
	registry              MediaLeaseRegistry
	owner                 string
	play                  PlaySessionID
	live                  LiveStreamID
	committedBeforeHeader bool
}

func TestResponseProjectionContextBatchesOnceAndOverlayIsPure(t *testing.T) {
	store := &projectionBatchStore{MemoryStore: NewMemoryStore()}
	server := NewServer(Config{GatewayServerID: "gateway-server"}, store)
	t.Cleanup(server.Close)
	if err := store.SaveItemChildCounts(context.Background(), []ItemChildCount{{ItemID: "series", ChildCount: 10}, {ItemID: "season", ChildCount: 5}}); err != nil {
		t.Fatal(err)
	}
	store.stateCalls, store.aggregateCalls, store.childCalls = 0, 0, 0
	data := []byte(`{"Items":[{"Id":"series","Type":"Series"},{"Id":"season","Type":"Season"},{"Id":"episode","Type":"Episode"}]}`)
	request := httptest.NewRequest(http.MethodGet, "https://gateway.test/emby/Items", nil)
	ctx, err := server.responseProjectionContextForDocument(request.Context(), request, &Session{GatewayUserID: "user", SyntheticUserID: "synthetic"}, upstreamRequestSnapshot{}, "", "", data, newResponseProjection(responseProjectionBaseItemEnvelope))
	if err != nil {
		t.Fatal(err)
	}
	if store.stateCalls != 1 || store.aggregateCalls != 1 || store.childCalls != 1 {
		t.Fatalf("preload calls state=%d aggregate=%d child=%d", store.stateCalls, store.aggregateCalls, store.childCalls)
	}
	if _, err := projectResponseDocument(data, newResponseProjection(responseProjectionBaseItemEnvelope), ctx); err != nil {
		t.Fatal(err)
	}
	if store.stateCalls != 1 || store.aggregateCalls != 1 || store.childCalls != 1 {
		t.Fatalf("overlay performed I/O: state=%d aggregate=%d child=%d", store.stateCalls, store.aggregateCalls, store.childCalls)
	}
}

type projectionBatchStore struct {
	*MemoryStore
	stateCalls     int
	aggregateCalls int
	childCalls     int
}

func (s *projectionBatchStore) ListPlaybackStatesByItemIDs(ctx context.Context, gatewayUserID string, itemIDs []string) (map[string]*PlaybackState, error) {
	s.stateCalls++
	return s.MemoryStore.ListPlaybackStatesByItemIDs(ctx, gatewayUserID, itemIDs)
}

func (s *projectionBatchStore) ListPlaybackAggregates(ctx context.Context, gatewayUserID string, seriesIDs, seasonIDs []string) (PlaybackAggregates, error) {
	s.aggregateCalls++
	return s.MemoryStore.ListPlaybackAggregates(ctx, gatewayUserID, seriesIDs, seasonIDs)
}

func (s *projectionBatchStore) ListItemChildCounts(ctx context.Context, itemIDs []string) (map[string]ItemChildCount, error) {
	s.childCalls++
	return s.MemoryStore.ListItemChildCounts(ctx, itemIDs)
}

func (w *leaseGateWriter) WriteHeader(status int) {
	_, err := w.registry.Validate(w.owner, w.play, w.live, time.Now())
	w.committedBeforeHeader = err == nil
	w.ResponseRecorder.WriteHeader(status)
}

type failingLeaseRegistry struct {
	MediaLeaseRegistry
	err error
}

func (r *failingLeaseRegistry) RegisterAll(string, []PlaySessionID, []LiveStreamID) error {
	return r.err
}

func TestResponseProjectionNeverGuessesBareArray(t *testing.T) {
	server, session := responseProjectionTestServer(t)
	response := writeProjectedTestResponse(server, session, http.MethodGet, "/Items/item", http.StatusOK, "application/json", `[{"Id":"item","ServerId":"backend-server"}]`, nil)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
	response = writeProjectedTestResponse(server, session, http.MethodGet, "/Items/item", http.StatusOK, "application/json", `{"Item":{"Id":"item","UserData":{"Played":true}}}`, nil)
	if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), `"Played":true`) {
		t.Fatalf("direct route guessed envelope semantics: status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestResponseProjectionFailureSanitizesHeaders(t *testing.T) {
	server, session := responseProjectionTestServer(t)
	headers := http.Header{
		"Content-Length":   {"999"},
		"Last-Modified":    {"yesterday"},
		"Content-MD5":      {"stale"},
		"Digest":           {"stale"},
		"Location":         {"https://backend.test/emby/Items/item"},
		"Content-Location": {"/Items/item"},
		"Set-Cookie":       {"backend=secret"},
		"Authorization":    {"Bearer backend-token"},
	}
	headers.Set("ETag", `"stale"`)
	response := writeProjectedTestResponse(server, session, http.MethodGet, "/Items/item", http.StatusOK, "application/json", `{"Id":`, headers)
	if response.Code != http.StatusBadGateway || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
	for _, name := range []string{"Content-Length", "ETag", "Last-Modified", "Content-MD5", "Digest", "Location", "Content-Location", "Set-Cookie", "Authorization"} {
		if response.Header().Get(name) != "" {
			t.Fatalf("stale header %s survived: %v", name, response.Header())
		}
	}
}

func TestResponseProjectionStatusMatrix(t *testing.T) {
	server, session := responseProjectionTestServer(t)
	t.Run("non-success JSON is not projected", func(t *testing.T) {
		body := `{"ServerId":"backend-server","Message":"unchanged"}`
		response := writeProjectedTestResponse(server, session, http.MethodGet, "/Items/item", http.StatusBadRequest, "application/json", body, nil)
		if response.Code != http.StatusBadRequest || response.Body.String() != body {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
	})
	t.Run("HEAD remains empty", func(t *testing.T) {
		response := writeProjectedTestResponse(server, session, http.MethodHead, "/Items/item", http.StatusOK, "application/json", `{"Id":"item"}`, nil)
		if response.Code != http.StatusOK || response.Body.Len() != 0 {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
	})
	t.Run("no content remains empty", func(t *testing.T) {
		response := writeProjectedTestResponse(server, session, http.MethodGet, "/Items/item", http.StatusNoContent, "application/json", `{"Id":"item"}`, nil)
		if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
	})
}

func TestResponseProjectionHeaderAndCredentialSafety(t *testing.T) {
	server, session := responseProjectionTestServer(t)
	headers := http.Header{
		"X-Plugin-Signature":      {"backend-user.backend-server"},
		"X-Emby-Token":            {"backend-token"},
		"X-MediaBrowser-Token":    {"backend-token"},
		"WWW-Authenticate":        {"Emby backend-token"},
		"Set-Cookie":              {"backend=secret"},
		"Location":                {"https://backend.test/emby/Items/item"},
		"X-Plugin-Cache-Metadata": {"backend-server"},
	}
	headers.Set("ETag", `"backend-user-backend-server"`)
	body := `{"Id":"item","Name":"backend-user and backend-server","UnknownUrl":"https://backend.test/emby/Items/item"}`
	response := writeProjectedTestResponse(server, session, http.MethodGet, "/Items/item/Images", http.StatusOK, "application/json", body, headers)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "backend-user and backend-server") {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	if response.Header().Get("ETag") != headers.Get("ETag") || response.Header().Get("X-Plugin-Signature") != headers.Get("X-Plugin-Signature") || response.Header().Get("X-Plugin-Cache-Metadata") != "backend-server" {
		t.Fatalf("opaque headers changed: %v", response.Header())
	}
	for _, name := range []string{"X-Emby-Token", "X-MediaBrowser-Token", "WWW-Authenticate", "Set-Cookie"} {
		if response.Header().Get(name) != "" {
			t.Fatalf("credential header %s survived: %v", name, response.Header())
		}
	}
	if response.Header().Get("Location") != "https://gateway.test/emby/Items/item" {
		t.Fatalf("Location=%q", response.Header().Get("Location"))
	}
	for _, leak := range []string{`{"Opaque":"backend-token"}`, `{"Opaque":"backend-\u0074oken"}`} {
		response := writeProjectedTestResponse(server, session, http.MethodGet, "/Items/item/Images", http.StatusOK, "application/json", leak, nil)
		if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), "backend-token") {
			t.Fatalf("unsafe response status=%d body=%q", response.Code, response.Body.String())
		}
	}
}

func TestResponseProjectionClearsValidatorsAfterProjection(t *testing.T) {
	server, session := responseProjectionTestServer(t)
	headers := http.Header{"Last-Modified": {"stale"}, "Content-Length": {"100"}}
	headers.Set("ETag", `"stale"`)
	headers.Set("Content-Encoding", "gzip")
	response := writeProjectedTestResponse(server, session, http.MethodGet, "/Items/item", http.StatusOK, "application/json", `{"Id":"item","ServerId":"backend-server"}`, headers)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ServerId":"gateway-server"`) {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("projected content type = %q", response.Header().Get("Content-Type"))
	}
	for _, name := range []string{"ETag", "Last-Modified", "Content-Length", "Content-Encoding"} {
		if response.Header().Get(name) != "" {
			t.Fatalf("validator %s survived: %v", name, response.Header())
		}
	}
}

func responseProjectionTestServer(t *testing.T) (*Server, *Session) {
	t.Helper()
	store := NewMemoryStore()
	server := NewServer(Config{GatewayBasePath: "/emby", GatewayServerID: "gateway-server", PublicBaseURL: "https://gateway.test/emby"}, store)
	t.Cleanup(server.Close)
	return server, &Session{GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user"}
}

func writeProjectedTestResponse(server *Server, session *Session, method, rel string, status int, contentType, body string, headers http.Header) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "https://gateway.test/emby"+rel, nil)
	responseRequest := request.Clone(request.Context())
	responseRequest.URL.Path = rel
	responseHeaders := headers.Clone()
	if responseHeaders == nil {
		responseHeaders = make(http.Header)
	}
	if contentType != "" {
		responseHeaders.Set("Content-Type", contentType)
	}
	response := &http.Response{StatusCode: status, Header: responseHeaders, Body: io.NopCloser(strings.NewReader(body)), Request: responseRequest, ContentLength: int64(len(body))}
	writer := httptest.NewRecorder()
	decision := routeclass.Classify(method, rel)
	server.writeProxyResponseWithProjection(writer, request, rel, response, session, upstreamRequestSnapshot{baseURL: "https://backend.test/emby", serverID: "backend-server", userID: "backend-user", token: "backend-token"}, "gateway-token", "https://gateway.test/emby", responseProjectionForRoute(method, rel, decision))
	return writer
}
