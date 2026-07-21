package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
)

func TestMetadataUpstreamRejectsBeforeDial(t *testing.T) {
	var dials atomic.Int32
	m := newMetadataUpstream(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		dials.Add(1)
		return nil, nil
	})}, nil, nil)
	snapshot := testMetadataSnapshot("token")
	session := &Session{SyntheticUserID: "synthetic"}
	tests := []struct {
		name string
		edit func(*http.Request)
	}{
		{"method", func(r *http.Request) { r.Method = http.MethodPost }},
		{"body", func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader("{}"))
			r.ContentLength = 2
		}},
		{"ownership", func(r *http.Request) {}},
		{"foreign path", func(r *http.Request) { r.URL.Path = "/Users/foreign/Items" }},
		{"foreign query", func(r *http.Request) { r.URL.RawQuery = "UserId=foreign" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := metadataRequest("/Items/item", "", session)
			ownership := routeclass.MetadataProxy
			if tt.name == "ownership" {
				ownership = routeclass.Unclassified
			}
			tt.edit(r)
			_, err := m.RoundTripMetadata(metadataUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: r, Session: session, Snapshot: snapshot}, Ownership: ownership})
			if err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("transport dials = %d, want 0", got)
	}
}

func TestMetadataUpstreamSanitizesAndUsesManagedIdentity(t *testing.T) {
	var got *http.Request
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer backend.Close()
	snapshot := testMetadataSnapshot("managed-token")
	snapshot.baseURL = backend.URL + "/emby"
	session := &Session{SyntheticUserID: "synthetic"}
	m := newMetadataUpstream(nil, nil, nil)
	r := metadataRequest("/Users/synthetic/Items", "UserId=synthetic&EnableUserData=true&Filters=IsPlayed,Genre", session)
	resp, err := (&metadataUpstream{client: m.client}).RoundTripMetadata(metadataUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: r, Session: session, Snapshot: snapshot}, Ownership: routeclass.MetadataProxy})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got == nil {
		t.Fatal("backend did not receive request")
	}
	q := got.URL.Query()
	// User-bound Items is pathBoundBaseItem: EnableUserData=false, no query UserId.
	if _, hasUserID := q["UserId"]; hasUserID || q.Get("EnableUserData") != "false" || q.Get("Filters") != "Genre" {
		t.Fatalf("sanitized query = %v", q)
	}
	if got.Header.Get("X-Emby-Token") != "managed-token" || strings.Contains(got.Header.Get("X-Emby-Authorization"), "gateway-token") {
		t.Fatalf("managed auth headers = %q / %q", got.Header.Get("X-Emby-Token"), got.Header.Get("X-Emby-Authorization"))
	}
}

func TestMetadataUpstreamPreservesRawOpaqueQueryAndSubstringIDs(t *testing.T) {
	var requestURI string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURI = r.RequestURI
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	snapshot := testMetadataSnapshot("backend-token")
	snapshot.baseURL = backend.URL
	session := &Session{SyntheticUserID: "synthetic"}
	req := metadataRequest("/Users/synthetic/Items/synthetic-copy", "sig=a%2Bb&dup=one&dup=two+words&opaque=synthetic&UserId=synthetic", session)
	resp, err := newMetadataUpstream(backend.Client(), nil, nil).RoundTripMetadata(metadataUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: session, Snapshot: snapshot}, Ownership: routeclass.MetadataProxy})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	want := "/Users/backend-user/Items/synthetic-copy?sig=a%2Bb&dup=one&dup=two+words&opaque=synthetic&EnableUserData=false"
	if requestURI != want {
		t.Fatalf("request URI=%q, want %q", requestURI, want)
	}
}

func TestMetadataUpstreamRejectsRedirectsAndRefreshesOnce(t *testing.T) {
	type observedRequest struct {
		path          string
		query         url.Values
		token         string
		authorization string
	}
	var firstRequests, secondRequests []observedRequest
	observeRequest := func(r *http.Request) observedRequest {
		return observedRequest{path: r.URL.Path, query: r.URL.Query(), token: r.Header.Get("X-Emby-Token"), authorization: r.Header.Get("X-Emby-Authorization")}
	}
	firstBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstRequests = append(firstRequests, observeRequest(r))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer firstBackend.Close()
	secondBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondRequests = append(secondRequests, observeRequest(r))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer secondBackend.Close()
	first := testMetadataSnapshot("old-token")
	first.baseURL = firstBackend.URL
	first.userID = "old-user"
	first.identity = backendIdentityForTest("old-device")
	second := first
	second.baseURL = secondBackend.URL
	second.userID = "new-user"
	second.token = "new-token"
	second.identity = backendIdentityForTest("new-device")
	var refreshes atomic.Int32
	m := newMetadataUpstream(nil, func(_ context.Context, got upstreamRequestSnapshot) (upstreamRequestSnapshot, bool, error) {
		refreshes.Add(1)
		if got.token != "old-token" {
			t.Fatal("refresh saw wrong token")
		}
		return second, true, nil
	}, nil)
	session := &Session{SyntheticUserID: "synthetic"}
	rawQuery := "UserId=synthetic&API_KEY=gateway-token&api_key=other-token&x-EMBY-token=header-token&signature=a%2Bb&Policy=catalog"
	request := metadataRequest("/Users/synthetic/Items", rawQuery, session)
	refreshedSnapshot := first
	resp, err := m.RoundTripMetadata(metadataUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: request, Session: session, Snapshot: first}, Ownership: routeclass.MetadataProxy, SnapshotRef: &refreshedSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(firstRequests) != 1 || len(secondRequests) != 1 || refreshes.Load() != 1 {
		t.Fatalf("backend calls/refreshes = %d/%d/%d", len(firstRequests), len(secondRequests), refreshes.Load())
	}
	assertRequest := func(got observedRequest, userID, token, deviceID string) {
		t.Helper()
		// Path-bound user Items: rewrite path identity, no query UserId, EnableUserData=false.
		if got.path != "/Users/"+userID+"/Items" {
			t.Fatalf("request path = %q, want user %q", got.path, userID)
		}
		if _, hasUserID := got.query["UserId"]; hasUserID || got.query.Get("EnableUserData") != "false" {
			t.Fatalf("path-bound query = %v", got.query)
		}
		for _, key := range []string{"API_KEY", "api_key", "x-EMBY-token"} {
			if _, ok := got.query[key]; ok {
				t.Fatalf("credential query %q remained: %v", key, got.query)
			}
		}
		if got.query.Get("signature") != "a+b" || got.query.Get("Policy") != "catalog" {
			t.Fatalf("catalog query changed: %v", got.query)
		}
		if got.token != token || !strings.Contains(got.authorization, `DeviceId="`+deviceID+`"`) || !strings.Contains(got.authorization, `UserId="`+userID+`"`) || !strings.Contains(got.authorization, `Token="`+token+`"`) {
			t.Fatalf("managed identity = token %q authorization %q", got.token, got.authorization)
		}
	}
	assertRequest(firstRequests[0], "old-user", "old-token", "old-device")
	assertRequest(secondRequests[0], "new-user", "new-token", "new-device")
	if request.URL.RawQuery != rawQuery {
		t.Fatalf("original query mutated: got %q want %q", request.URL.RawQuery, rawQuery)
	}
	if refreshedSnapshot.baseURL != second.baseURL || refreshedSnapshot.userID != second.userID || refreshedSnapshot.token != second.token || refreshedSnapshot.identity.DeviceID != second.identity.DeviceID {
		t.Fatalf("snapshot reference not refreshed: %#v", refreshedSnapshot)
	}
}

func TestMetadataUpstreamReportsRefreshResultAndClosesDiscardedUnauthorizedOnce(t *testing.T) {
	refreshFailure := errors.New("refresh failed")
	for _, tt := range []struct {
		name       string
		confirmed  bool
		refreshErr error
		wantCalls  int
	}{
		{name: "success", confirmed: true, wantCalls: 2},
		{name: "failure", confirmed: true, refreshErr: refreshFailure, wantCalls: 1},
		{name: "unconfirmed", wantCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls int
			firstBody := &adapterCloseCountingBody{Reader: strings.NewReader("unauthorized")}
			finalBody := &adapterCloseCountingBody{Reader: strings.NewReader("ok")}
			var results []upstreamRefreshResult
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls == 2 && len(results) != 1 {
					t.Fatal("retry started before refresh result notification")
				}
				status, body := http.StatusUnauthorized, io.ReadCloser(firstBody)
				if calls == 2 {
					status, body = http.StatusOK, finalBody
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: body, Request: req}, nil
			})}
			first := testMetadataSnapshot("old-token")
			second := first
			second.token = "new-token"
			adapter := newMetadataUpstream(client, func(context.Context, upstreamRequestSnapshot) (upstreamRequestSnapshot, bool, error) {
				return second, tt.confirmed, tt.refreshErr
			}, nil)
			in := metadataUpstreamRequest{
				upstreamHTTPRequest: upstreamHTTPRequest{
					Request: metadataRequest("/Users/synthetic/Items", "", &Session{SyntheticUserID: "synthetic"}),
					Session: &Session{SyntheticUserID: "synthetic"}, Snapshot: first,
					refreshResult: func(result upstreamRefreshResult) { results = append(results, result) },
				},
				Ownership: routeclass.MetadataProxy,
			}
			resp, err := adapter.RoundTripMetadata(in)
			if err != nil {
				t.Fatal(err)
			}
			if calls != tt.wantCalls || len(results) != 1 || results[0].Confirmed != tt.confirmed || !errors.Is(results[0].Err, tt.refreshErr) {
				t.Fatalf("calls=%d results=%+v", calls, results)
			}
			if tt.wantCalls == 2 && firstBody.closes != 1 {
				t.Fatalf("discarded unauthorized closes=%d, want 1", firstBody.closes)
			}
			_ = resp.Body.Close()
			_ = resp.Body.Close()
			if tt.wantCalls == 2 && finalBody.closes != 1 {
				t.Fatalf("final response closes=%d, want 1", finalBody.closes)
			}
			if tt.wantCalls == 1 && firstBody.closes != 1 {
				t.Fatalf("returned unauthorized closes=%d, want 1", firstBody.closes)
			}
		})
	}
}

func TestMetadataUpstreamRejectsSameAndCrossOriginRedirects(t *testing.T) {
	for _, crossOrigin := range []bool{false, true} {
		var targetCalls atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			targetCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			location := r.URL.String()
			if crossOrigin {
				location = target.URL + "/Items/target"
			}
			w.Header().Set("Location", location)
			w.WriteHeader(http.StatusFound)
		}))
		m := newMetadataUpstream(nil, nil, nil)
		snapshot := testMetadataSnapshot("token")
		snapshot.baseURL = origin.URL
		session := &Session{SyntheticUserID: "synthetic"}
		resp, err := m.RoundTripMetadata(metadataUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: metadataRequest("/Items/redirect", "", session), Session: session, Snapshot: snapshot}, Ownership: routeclass.MetadataProxy})
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err == nil {
			t.Fatalf("crossOrigin=%v: expected redirect rejection", crossOrigin)
		}
		if targetCalls.Load() != 0 {
			t.Fatalf("crossOrigin=%v: redirect target was called", crossOrigin)
		}
		origin.Close()
		target.Close()
	}
}

func TestMetadataUpstreamTelemetryContainsNoSecrets(t *testing.T) {
	var event observe.Event
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer backend.Close()
	snapshot := testMetadataSnapshot("secret-token")
	snapshot.baseURL = backend.URL
	m := newMetadataUpstream(nil, nil, func(got observe.Event) { event = got })
	session := &Session{SyntheticUserID: "synthetic-user"}
	r := metadataRequest("/Items/item", "UserId=synthetic-user&token=secret-token", session)
	resp, err := m.RoundTripMetadata(metadataUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: r, Session: session, Snapshot: snapshot}, Ownership: routeclass.MetadataProxy})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	for _, secret := range []string{"secret-token", "synthetic-user", "backend-user"} {
		if strings.Contains(event.ErrorKind, secret) {
			t.Fatalf("telemetry leaked %q: %#v", secret, event)
		}
	}
	if event.Kind != observe.KindUpstreamRequest || event.RouteClass != observe.RouteMetadata || event.StatusClass != observe.Status2xx || event.DurationMS < 0 {
		t.Fatalf("telemetry = %#v", event)
	}
}

func TestMetadataUpstreamAcceptsWrappedZeroLengthBody(t *testing.T) {
	var dials atomic.Int32
	var got *http.Request
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dials.Add(1)
		got = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Id":"backend-server"}`)
	}))
	defer backend.Close()
	snapshot := testMetadataSnapshot("managed-token")
	snapshot.baseURL = backend.URL
	session := &Session{SyntheticUserID: "synthetic"}
	// Simulate PocketBase *router.RereadableReadCloser around an empty GET body.
	req := metadataRequest("/System/Info", "api_key=gateway-token&X-Emby-Client=Emby+Web", session)
	req.Body = io.NopCloser(strings.NewReader(""))
	req.ContentLength = 0
	resp, err := newMetadataUpstream(backend.Client(), nil, nil).RoundTripMetadata(metadataUpstreamRequest{
		upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: session, Snapshot: snapshot},
		Ownership:           routeclass.MetadataProxy,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if dials.Load() != 1 || got == nil || got.URL.Path != "/System/Info" || got.URL.RawQuery != "" {
		t.Fatalf("backend dials=%d request=%v", dials.Load(), got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestMetadataUpstreamRejectsDeclaredBodiesBeforeDial(t *testing.T) {
	var dials atomic.Int32
	adapter := newMetadataUpstream(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		dials.Add(1)
		return nil, nil
	})}, nil, nil)
	snapshot := testMetadataSnapshot("token")
	session := &Session{SyntheticUserID: "synthetic"}
	tests := []struct {
		name string
		edit func(*http.Request)
	}{
		{"positive length", func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader("{}"))
			r.ContentLength = 2
		}},
		{"unknown length", func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(""))
			r.ContentLength = -1
		}},
		{"chunked transfer encoding", func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(""))
			r.ContentLength = 0
			r.TransferEncoding = []string{"chunked"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := metadataRequest("/System/Info", "", session)
			tt.edit(req)
			_, err := adapter.RoundTripMetadata(metadataUpstreamRequest{
				upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: session, Snapshot: snapshot},
				Ownership:           routeclass.MetadataProxy,
			})
			if !errors.Is(err, ErrBadRequest) {
				t.Fatalf("error = %v, want ErrBadRequest", err)
			}
		})
	}
	if dials.Load() != 0 {
		t.Fatalf("dials = %d, want 0", dials.Load())
	}
}

func TestMetadataUpstreamServerWrappedEmptyBodySystemInfoAndViews(t *testing.T) {
	// Narrow server-level regression: PocketBase-style wrapped empty GET bodies
	// must not fail System/Info or Views solely due to Body != nil/NoBody.
	var paths []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		switch r.URL.Path {
		case "/emby/System/Info":
			if r.URL.RawQuery != "" {
				t.Fatalf("SystemInfo RawQuery = %q, want empty", r.URL.RawQuery)
			}
			writeTestJSON(w, map[string]any{"Id": "backend-server", "ServerName": "Real Emby"})
		case "/emby/Users/backend-user/Views":
			if _, ok := r.URL.Query()["UserId"]; ok || r.URL.Query().Get("EnableUserData") != "" {
				t.Fatalf("Views query leaked user fields: %v", r.URL.Query())
			}
			writeTestJSON(w, map[string]any{"Items": []any{}, "TotalRecordCount": 0})
		default:
			t.Fatalf("unexpected backend %s %s", r.Method, r.URL.String())
		}
	}))
	defer backend.Close()

	store := testStore(backend.URL + "/emby")
	session := testSession()
	store.Sessions[HashToken("gateway-token")] = session
	server := NewServer(Config{GatewayBasePath: "/emby", GatewayServerID: "gateway-server", HTTPClient: backend.Client()}, store)

	for _, path := range []string{"/emby/System/Info", "/emby/Users/gateway-user/Views"} {
		req := httptest.NewRequest(http.MethodGet, "http://gateway.test"+path+"?api_key=gateway-token&X-Emby-Client=Emby+Web", nil)
		// PocketBase wraps empty bodies; ContentLength stays 0 with no TE.
		req.Body = io.NopCloser(strings.NewReader(""))
		req.ContentLength = 0
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%q", path, rec.Code, rec.Body.String())
		}
	}
	if len(paths) != 2 || !strings.HasPrefix(paths[0], "/emby/System/Info?") || !strings.HasPrefix(paths[1], "/emby/Users/backend-user/Views?") {
		t.Fatalf("backend paths = %#v", paths)
	}
}

func TestMetadataUpstreamPublicModeUsesIdentityWithoutManagedCredentials(t *testing.T) {
	var got *http.Request
	var event observe.Event
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		_, _ = io.WriteString(w, `{"Id":"backend-server"}`)
	}))
	defer backend.Close()
	snapshot := upstreamRequestSnapshot{baseURL: backend.URL, serverID: "backend-server", identity: backendIdentityForTest("public-device")}
	adapter := newMetadataUpstream(backend.Client(), nil, func(value observe.Event) { event = value })
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/System/Info/Public", nil)
	resp, err := adapter.RoundTripMetadata(metadataUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Snapshot: snapshot}, Internal: true, Public: true})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got == nil || got.Header.Get("X-Emby-Token") != "" || strings.Contains(got.Header.Get("X-Emby-Authorization"), "Token=") || strings.Contains(got.Header.Get("X-Emby-Authorization"), "UserId=") {
		t.Fatalf("public headers=%#v", got.Header)
	}
	if event.Kind != observe.KindUpstreamRequest || event.RouteClass != observe.RouteMetadata {
		t.Fatalf("event=%#v", event)
	}
}

func TestMetadataQueryPolicyForRequestMapping(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   metadataQueryPolicy
	}{
		{http.MethodGet, "/System/Info", metadataQueryPolicySystemInfo},
		{http.MethodHead, "/System/Info", metadataQueryPolicySystemInfo},
		{http.MethodGet, "/Users/synthetic/Views", metadataQueryPolicyPathBoundNeutral},
		{http.MethodGet, "/Users/synthetic/HomeSections", metadataQueryPolicyPathBoundNeutral},
		{http.MethodGet, "/Users/synthetic/Items", metadataQueryPolicyPathBoundBaseItem},
		{http.MethodGet, "/Users/synthetic/Items/item-1", metadataQueryPolicyPathBoundBaseItem},
		{http.MethodGet, "/Users/synthetic/Items/Resume", metadataQueryPolicyPathBoundBaseItem},
		{http.MethodGet, "/Users/synthetic/Items/Latest", metadataQueryPolicyPathBoundBaseItem},
		{http.MethodGet, "/Users/synthetic/Items/item-1/Intros", metadataQueryPolicyPathBoundBaseItem},
		{http.MethodGet, "/Users/synthetic/Items/item-1/LocalTrailers", metadataQueryPolicyPathBoundBaseItem},
		{http.MethodGet, "/Users/synthetic/Items/item-1/SpecialFeatures", metadataQueryPolicyPathBoundBaseItem},
		{http.MethodGet, "/Users/synthetic/Suggestions", metadataQueryPolicyPathBoundBaseItem},
		{http.MethodGet, "/Users/synthetic/Sections/home/Items", metadataQueryPolicyPathBoundBaseItem},
		{http.MethodGet, "/Items", metadataQueryPolicyGlobalBaseItem},
		{http.MethodGet, "/Items/item-1", metadataQueryPolicyGlobalBaseItem},
		{http.MethodGet, "/Items/item-1/Similar", metadataQueryPolicyGlobalBaseItem},
		{http.MethodGet, "/Items/item-1/ThemeMedia", metadataQueryPolicyGlobalBaseItem},
		{http.MethodGet, "/Items/item-1/Ancestors", metadataQueryPolicyGlobalBaseItem},
		{http.MethodGet, "/Movies/Recommendations", metadataQueryPolicyGlobalBaseItem},
		{http.MethodGet, "/Artists/name", metadataQueryPolicyGlobalBaseItem},
		{http.MethodGet, "/Shows/show/Episodes", metadataQueryPolicyGlobalBaseItem},
		// Opaque/static before generic item/by-name: reserved and image metadata.
		{http.MethodGet, "/Items/Counts", metadataQueryPolicyNonBaseItem},
		{http.MethodGet, "/Items/Prefixes", metadataQueryPolicyNonBaseItem},
		{http.MethodGet, "/Items/item-1/Images", metadataQueryPolicyNonBaseItem},
		{http.MethodGet, "/Artists/Prefixes", metadataQueryPolicyNonBaseItem},
		{http.MethodGet, "/ScheduledTasks", metadataQueryPolicyNonBaseItem},
		{http.MethodPost, "/System/Info", metadataQueryPolicyNonBaseItem},
	}
	for _, tt := range tests {
		if got := metadataQueryPolicyForRequest(tt.method, tt.path); got != tt.want {
			t.Fatalf("%s %s = %v, want %v", tt.method, tt.path, got, tt.want)
		}
	}
}

func TestMetadataUpstreamSystemInfoEmitsEmptyQueryAndSucceeds(t *testing.T) {
	var got *http.Request
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Id":"backend-server"}`)
	}))
	defer backend.Close()
	snapshot := testMetadataSnapshot("managed-token")
	snapshot.baseURL = backend.URL
	session := &Session{SyntheticUserID: "synthetic"}
	// Observed Emby Web System/Info query shape.
	raw := strings.Join([]string{
		"api_key=gateway-token",
		"X-Emby-Client=Emby+Web",
		"X-Emby-Device-Name=Chrome",
		"X-Emby-Device-Id=device-abc",
		"X-Emby-Client-Version=4.9.5.0",
		"X-Emby-Language=en-us",
		"X-Emby-Token=gateway-token",
	}, "&")
	req := metadataRequest("/System/Info", raw, session)
	req.Header = http.Header{"X-Emby-Token": []string{"gateway-token"}}
	resp, err := newMetadataUpstream(backend.Client(), nil, nil).RoundTripMetadata(metadataUpstreamRequest{
		upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: session, Snapshot: snapshot},
		Ownership:           routeclass.MetadataProxy,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got == nil || got.URL.Path != "/System/Info" || got.URL.RawQuery != "" {
		t.Fatalf("backend request path/query = %q %q", got.URL.Path, got.URL.RawQuery)
	}
}

func TestMetadataUpstreamViewsRewritesPathPreservesNeutralsNoUserFields(t *testing.T) {
	var got *http.Request
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Items":[],"TotalRecordCount":0}`)
	}))
	defer backend.Close()
	snapshot := testMetadataSnapshot("managed-token")
	snapshot.baseURL = backend.URL
	session := &Session{SyntheticUserID: "synthetic"}
	raw := strings.Join([]string{
		"UserId=synthetic",
		"api_key=gateway-token",
		"X-Emby-Client=Emby+Web",
		"X-Emby-Device-Id=device-abc",
		"X-Emby-Language=en-us",
		"IncludeExternalContent=false",
		"EnableUserData=true",
	}, "&")
	req := metadataRequest("/Users/synthetic/Views", raw, session)
	resp, err := newMetadataUpstream(backend.Client(), nil, nil).RoundTripMetadata(metadataUpstreamRequest{
		upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: session, Snapshot: snapshot},
		Ownership:           routeclass.MetadataProxy,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got == nil || got.URL.Path != "/Users/backend-user/Views" {
		t.Fatalf("backend path = %v", got)
	}
	q := got.URL.Query()
	if _, hasUserID := q["UserId"]; hasUserID || q.Get("EnableUserData") != "" || q.Get("api_key") != "" {
		t.Fatalf("Views must not keep/append user or credential fields: %v", q)
	}
	if q.Get("X-Emby-Client") != "Emby Web" || q.Get("X-Emby-Device-Id") != "device-abc" || q.Get("X-Emby-Language") != "en-us" || q.Get("IncludeExternalContent") != "false" {
		t.Fatalf("neutral client fields not preserved: %v", q)
	}
}

func TestMetadataUpstreamPolicyShapesForUserGlobalAndImageRoutes(t *testing.T) {
	type capture struct {
		path  string
		query url.Values
	}
	var got capture
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = capture{path: r.URL.Path, query: r.URL.Query()}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer backend.Close()
	snapshot := testMetadataSnapshot("managed-token")
	snapshot.baseURL = backend.URL
	session := &Session{SyntheticUserID: "synthetic"}
	adapter := newMetadataUpstream(backend.Client(), nil, nil)

	roundTrip := func(path, raw string) {
		t.Helper()
		req := metadataRequest(path, raw, session)
		resp, err := adapter.RoundTripMetadata(metadataUpstreamRequest{
			upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: session, Snapshot: snapshot},
			Ownership:           routeclass.MetadataProxy,
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	// User Items: pathBoundBaseItem.
	roundTrip("/Users/synthetic/Items", "UserId=synthetic&Recursive=true&EnableUserData=true")
	if got.path != "/Users/backend-user/Items" || got.query.Get("EnableUserData") != "false" || got.query.Get("Recursive") != "true" {
		t.Fatalf("user Items = path %q query %v", got.path, got.query)
	}
	if _, ok := got.query["UserId"]; ok {
		t.Fatalf("user Items must not append query UserId: %v", got.query)
	}

	// User direct item.
	roundTrip("/Users/synthetic/Items/item-1", "UserId=synthetic&Fields=Path,UserData")
	if got.path != "/Users/backend-user/Items/item-1" || got.query.Get("EnableUserData") != "false" || got.query.Get("Fields") != "Path" {
		t.Fatalf("user direct item = path %q query %v", got.path, got.query)
	}
	if _, ok := got.query["UserId"]; ok {
		t.Fatalf("user direct item must not append query UserId: %v", got.query)
	}

	// Suggestions and Sections.
	roundTrip("/Users/synthetic/Suggestions", "UserId=synthetic&Limit=10")
	if got.path != "/Users/backend-user/Suggestions" || got.query.Get("EnableUserData") != "false" || got.query.Get("Limit") != "10" {
		t.Fatalf("suggestions = path %q query %v", got.path, got.query)
	}
	if _, ok := got.query["UserId"]; ok {
		t.Fatalf("suggestions must not append query UserId: %v", got.query)
	}
	roundTrip("/Users/synthetic/Sections/home/Items", "UserId=synthetic&StartIndex=0")
	if got.path != "/Users/backend-user/Sections/home/Items" || got.query.Get("EnableUserData") != "false" || got.query.Get("StartIndex") != "0" {
		t.Fatalf("section items = path %q query %v", got.path, got.query)
	}
	if _, ok := got.query["UserId"]; ok {
		t.Fatalf("section items must not append query UserId: %v", got.query)
	}

	// Global Items: backend UserId + EnableUserData=false.
	roundTrip("/Items", "UserId=synthetic&StartIndex=0&Limit=50")
	if got.path != "/Items" || got.query.Get("UserId") != "backend-user" || got.query.Get("EnableUserData") != "false" || got.query.Get("Limit") != "50" {
		t.Fatalf("global Items = path %q query %v", got.path, got.query)
	}

	// Image metadata: neither UserId nor EnableUserData.
	roundTrip("/Items/item-1/Images", "maxWidth=400&tag=abc%2Bdef&UserId=synthetic&api_key=gateway-token&EnableUserData=true")
	if got.path != "/Items/item-1/Images" || got.query.Get("maxWidth") != "400" || got.query.Get("tag") != "abc+def" {
		t.Fatalf("image metadata = path %q query %v", got.path, got.query)
	}
	if _, ok := got.query["UserId"]; ok || got.query.Get("EnableUserData") != "" || got.query.Get("api_key") != "" {
		t.Fatalf("image metadata must not keep/append user or credential fields: %v", got.query)
	}
}

func TestMetadataUpstreamForeignUserIdRejectedBeforeDial(t *testing.T) {
	var dials atomic.Int32
	adapter := newMetadataUpstream(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		dials.Add(1)
		return nil, nil
	})}, nil, nil)
	snapshot := testMetadataSnapshot("token")
	session := &Session{SyntheticUserID: "synthetic"}
	for _, path := range []string{"/System/Info", "/Users/synthetic/Views", "/Users/synthetic/Items", "/Items", "/Items/item/Images"} {
		req := metadataRequest(path, "UserId=foreign", session)
		_, err := adapter.RoundTripMetadata(metadataUpstreamRequest{
			upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: session, Snapshot: snapshot},
			Ownership:           routeclass.MetadataProxy,
		})
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s error = %v, want ErrForbidden", path, err)
		}
	}
	if dials.Load() != 0 {
		t.Fatalf("dials = %d, want 0", dials.Load())
	}
}

func TestMetadataUpstreamRefreshRetryPreservesPolicyAndSnapshotIdentity(t *testing.T) {
	type observed struct {
		path  string
		raw   string
		token string
		auth  string
	}
	var first, second []observed
	firstBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first = append(first, observed{path: r.URL.Path, raw: r.URL.RawQuery, token: r.Header.Get("X-Emby-Token"), auth: r.Header.Get("X-Emby-Authorization")})
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer firstBackend.Close()
	secondBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		second = append(second, observed{path: r.URL.Path, raw: r.URL.RawQuery, token: r.Header.Get("X-Emby-Token"), auth: r.Header.Get("X-Emby-Authorization")})
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer secondBackend.Close()

	oldSnap := testMetadataSnapshot("old-token")
	oldSnap.baseURL = firstBackend.URL
	oldSnap.userID = "old-user"
	oldSnap.identity = backendIdentityForTest("old-device")
	newSnap := oldSnap
	newSnap.baseURL = secondBackend.URL
	newSnap.userID = "new-user"
	newSnap.token = "new-token"
	newSnap.identity = backendIdentityForTest("new-device")

	adapter := newMetadataUpstream(nil, func(_ context.Context, got upstreamRequestSnapshot) (upstreamRequestSnapshot, bool, error) {
		if got.token != "old-token" || got.userID != "old-user" {
			t.Fatalf("refresh snapshot = %#v", got)
		}
		return newSnap, true, nil
	}, nil)
	session := &Session{SyntheticUserID: "synthetic"}
	// Global Items policy must recompute on retry with refreshed backend UserId.
	raw := "UserId=synthetic&StartIndex=0&api_key=gateway-token&X-Emby-Client=Emby+Web"
	req := metadataRequest("/Items", raw, session)
	refreshed := oldSnap
	resp, err := adapter.RoundTripMetadata(metadataUpstreamRequest{
		upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: session, Snapshot: oldSnap},
		Ownership:           routeclass.MetadataProxy,
		SnapshotRef:         &refreshed,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("backend calls = %d/%d", len(first), len(second))
	}
	assertGlobal := func(got observed, userID, token, device string) {
		t.Helper()
		if got.path != "/Items" {
			t.Fatalf("path = %q", got.path)
		}
		q, err := url.ParseQuery(got.raw)
		if err != nil {
			t.Fatal(err)
		}
		if q.Get("UserId") != userID || q.Get("EnableUserData") != "false" || q.Get("StartIndex") != "0" || q.Get("X-Emby-Client") != "Emby Web" {
			t.Fatalf("query = %v want user %q", q, userID)
		}
		if _, ok := q["api_key"]; ok {
			t.Fatalf("credential remained: %v", q)
		}
		if got.token != token || !strings.Contains(got.auth, `UserId="`+userID+`"`) || !strings.Contains(got.auth, `DeviceId="`+device+`"`) || !strings.Contains(got.auth, `Token="`+token+`"`) {
			t.Fatalf("identity token=%q auth=%q", got.token, got.auth)
		}
	}
	assertGlobal(first[0], "old-user", "old-token", "old-device")
	assertGlobal(second[0], "new-user", "new-token", "new-device")
	if req.URL.RawQuery != raw {
		t.Fatalf("original query mutated: %q", req.URL.RawQuery)
	}
	if refreshed.userID != "new-user" || refreshed.token != "new-token" || refreshed.baseURL != secondBackend.URL {
		t.Fatalf("snapshot ref not refreshed: %#v", refreshed)
	}
}

func TestMetadataUpstreamPublicSystemInfoBypassesQueryPolicy(t *testing.T) {
	var got *http.Request
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		_, _ = io.WriteString(w, `{"Id":"backend-server"}`)
	}))
	defer backend.Close()
	snapshot := upstreamRequestSnapshot{baseURL: backend.URL, serverID: "backend-server", identity: backendIdentityForTest("public-device")}
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/System/Info/Public", nil)
	resp, err := newMetadataUpstream(backend.Client(), nil, nil).RoundTripMetadata(metadataUpstreamRequest{
		upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Snapshot: snapshot},
		Internal:            true,
		Public:              true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got == nil || got.URL.Path != "/System/Info/Public" || got.URL.RawQuery != "" {
		t.Fatalf("public request = %#v", got)
	}
	if got.Header.Get("X-Emby-Token") != "" {
		t.Fatalf("public must not send managed token: %q", got.Header.Get("X-Emby-Token"))
	}
}

func metadataRequest(path, rawQuery string, session *Session) *http.Request {
	return &http.Request{Method: http.MethodGet, URL: &url.URL{Path: path, RawQuery: rawQuery}, Body: nil, Header: make(http.Header)}
}

func testMetadataSnapshot(token string) upstreamRequestSnapshot {
	return upstreamRequestSnapshot{baseURL: "http://backend.invalid/emby", userID: "backend-user", token: token, identity: DefaultBackendClientIdentity()}
}

type adapterCloseCountingBody struct {
	io.Reader
	closes int
}

func (b *adapterCloseCountingBody) Close() error {
	b.closes++
	return nil
}
