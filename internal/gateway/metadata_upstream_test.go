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
		{"body", func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader("{}")) }},
		{"ownership", func(r *http.Request) {}},
		{"foreign path", func(r *http.Request) { r.URL.Path = "/Users/foreign/Items" }},
		{"foreign query", func(r *http.Request) { r.URL.RawQuery = "UserId=foreign" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := metadataRequest("/Items/item", "", session)
			ownership := routeclass.MetadataProxy
			if tt.name == "ownership" {
				ownership = routeclass.LegacyProxy
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
	if q.Get("UserId") != "backend-user" || q.Get("EnableUserData") != "false" || q.Get("Filters") != "Genre" {
		t.Fatalf("sanitized query = %v", q)
	}
	if got.Header.Get("X-Emby-Token") != "managed-token" || strings.Contains(got.Header.Get("X-Emby-Authorization"), "gateway-token") {
		t.Fatalf("managed auth headers = %q / %q", got.Header.Get("X-Emby-Token"), got.Header.Get("X-Emby-Authorization"))
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
		if got.path != "/Users/"+userID+"/Items" || got.query.Get("UserId") != userID {
			t.Fatalf("request identity = path %q query %v, want user %q", got.path, got.query, userID)
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

func metadataRequest(path, rawQuery string, session *Session) *http.Request {
	return &http.Request{Method: http.MethodGet, URL: &url.URL{Path: path, RawQuery: rawQuery}, Body: nil}
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
