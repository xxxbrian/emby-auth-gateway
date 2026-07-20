package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

type personalPlanHTTPStore struct {
	*MemoryStore
	listErr    error
	listCalls  int
	batchCalls int
}

func (s *personalPlanHTTPStore) ListPlaybackStates(ctx context.Context, userID string, filter PlaybackStateFilter) ([]PlaybackState, error) {
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.MemoryStore.ListPlaybackStates(ctx, userID, filter)
}

func (s *personalPlanHTTPStore) ListPlaybackStatesByItemIDs(ctx context.Context, userID string, itemIDs []string) (map[string]*PlaybackState, error) {
	s.batchCalls++
	return s.MemoryStore.ListPlaybackStatesByItemIDs(ctx, userID, itemIDs)
}

func personalPlanHTTPServerWithStore(t *testing.T, store Store, base *MemoryStore, fake *personalPlanSourceMetadataFake) (*Server, *phase5AuthSpy) {
	t.Helper()
	server := NewServer(Config{GatewayBasePath: "/emby", HTTPClient: &http.Client{Transport: phase5PanicTransport{}}}, store)
	runtime, err := base.LoadDefaultUpstreamRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	auth := &phase5AuthSpy{runtime: runtime}
	server.managedAuthUpstream = auth
	server.metadataUpstream = fake
	return server, auth
}

func personalPlanHTTPServer(t *testing.T, fake *personalPlanSourceMetadataFake) (*Server, *MemoryStore, *phase5AuthSpy) {
	t.Helper()
	server, store, auth, _, _, _ := phase5DispatchServer(t)
	server.metadataUpstream = fake
	return server, store, auth
}

func personalPlanHTTPRequest(server *Server, target string) *httptest.ResponseRecorder {
	writer := httptest.NewRecorder()
	server.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby"+target, nil))
	return writer
}

func TestPersonalPlanHTTPRejectsInvalidQueryAndIdentityBeforeEgress(t *testing.T) {
	tests := []struct {
		name   string
		target string
		status int
	}{
		{"malformed bool", "/Items?api_key=gateway-token&IsPlayed=maybe", http.StatusBadRequest},
		{"malformed int", "/Items?api_key=gateway-token&IsFavorite=true&Limit=nope", http.StatusBadRequest},
		{"duplicate alias", "/Items?api_key=gateway-token&IsFavorite=true&isfavorite=true", http.StatusBadRequest},
		{"unknown filter", "/Items?api_key=gateway-token&Filters=NoSuchFilter", http.StatusBadRequest},
		{"foreign path", "/Users/foreign/Items?api_key=gateway-token&IsFavorite=true", http.StatusForbidden},
		{"duplicate UserId", "/Items?api_key=gateway-token&UserId=gateway-user&UserId=gateway-user&IsFavorite=true", http.StatusBadRequest},
		{"case variant UserId", "/Items?api_key=gateway-token&UserId=gateway-user&userid=gateway-user&IsFavorite=true", http.StatusBadRequest},
		{"foreign query", "/Items?api_key=gateway-token&UserId=foreign&IsFavorite=true", http.StatusForbidden},
		{"unsupported path", "/System/Info?api_key=gateway-token&Filters=Likes", http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &personalPlanSourceMetadataFake{}
			server, _, auth := personalPlanHTTPServer(t, fake)
			defer server.Close()
			response := personalPlanHTTPRequest(server, test.target)
			if response.Code != test.status || fake.calls != 0 || auth.ensure != 0 {
				t.Fatalf("status=%d metadata=%d ensure=%d body=%q", response.Code, fake.calls, auth.ensure, response.Body.String())
			}
		})
	}
}

func TestPersonalPlanHTTPPassthroughPreservesShapeAndSuppliedPagination(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"document total", `{"Items":[],"TotalRecordCount":7,"StartIndex":2,"Marker":"kept","Big":9223372036854775808123}`, http.StatusOK},
		{"document absent total", `{"Items":[],"Marker":"kept"}`, http.StatusOK},
		{"undeclared array", `[{"Id":"one"}]`, http.StatusBadGateway},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: test.body, snapshot: personalPlanSourceUpstreamSnapshot()}
			server, _, _ := personalPlanHTTPServer(t, fake)
			defer server.Close()
			response := personalPlanHTTPRequest(server, "/Items?api_key=gateway-token")
			if response.Code != test.wantStatus || fake.calls != 1 {
				t.Fatalf("status=%d calls=%d body=%q", response.Code, fake.calls, response.Body.String())
			}
			if test.wantStatus != http.StatusOK {
				return
			}
			var want, got any
			if json.Unmarshal([]byte(test.body), &want) != nil || json.Unmarshal(response.Body.Bytes(), &got) != nil {
				t.Fatal("invalid JSON")
			}
			wantMap, wantObject := want.(map[string]any)
			gotMap, gotObject := got.(map[string]any)
			if wantObject != gotObject {
				t.Fatalf("shape changed: want object=%v got object=%v", wantObject, gotObject)
			}
			if wantObject {
				for _, key := range []string{"TotalRecordCount", "StartIndex", "Marker"} {
					if wantMap[key] != gotMap[key] {
						t.Fatalf("%s=%v, want %v", key, gotMap[key], wantMap[key])
					}
				}
			}
			if strings.Contains(test.body, "9223372036854775808123") && !strings.Contains(response.Body.String(), "9223372036854775808123") {
				t.Fatalf("large integer changed: %q", response.Body.String())
			}
		})
	}
}

func TestPersonalPlanHTTPPassthroughRejectsMalformedSuppliedTotal(t *testing.T) {
	for _, body := range []string{
		`{"Items":[],"TotalRecordCount":"7"}`,
		`{"Items":[],"StartIndex":-1}`,
		`{"Items":[],"TotalRecordCount":1.5}`,
	} {
		fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: body, snapshot: personalPlanSourceUpstreamSnapshot()}
		server, _, _ := personalPlanHTTPServer(t, fake)
		response := personalPlanHTTPRequest(server, "/Items?api_key=gateway-token")
		server.Close()
		if response.Code != http.StatusBadGateway || fake.calls != 1 || strings.HasPrefix(strings.TrimSpace(response.Body.String()), "{") {
			t.Fatalf("status=%d calls=%d body=%q", response.Code, fake.calls, response.Body.String())
		}
	}
}

func TestPersonalPlanHTTPLatestZeroDoesNoStoreOrUpstreamWork(t *testing.T) {
	fake := &personalPlanSourceMetadataFake{}
	server, _, auth := personalPlanHTTPServer(t, fake)
	defer server.Close()
	response := personalPlanHTTPRequest(server, "/Users/gateway-user/Items/Latest?api_key=gateway-token&Limit=0")
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" || fake.calls != 0 || auth.ensure != 0 {
		t.Fatalf("status=%d body=%q metadata=%d ensure=%d", response.Code, response.Body.String(), fake.calls, auth.ensure)
	}
}

func TestPersonalPlanHTTPPositiveTotalBeforeLimitZeroAndResumeDoesNotGroupSeries(t *testing.T) {
	t.Run("positive limit zero", func(t *testing.T) {
		fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: `{"Items":[{"Id":"one","Type":"Episode","SeriesId":"series"},{"Id":"two","Type":"Episode","SeriesId":"series"}]}`, snapshot: personalPlanSourceUpstreamSnapshot()}
		server, store, _ := personalPlanHTTPServer(t, fake)
		defer server.Close()
		for _, id := range []string{"one", "two"} {
			if err := store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: id, SeriesID: "series", IsFavorite: true}); err != nil {
				t.Fatal(err)
			}
		}
		response := personalPlanHTTPRequest(server, "/Items?api_key=gateway-token&IsFavorite=true&Limit=0")
		var result struct {
			Items            []any
			TotalRecordCount int
			StartIndex       int
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Items) != 0 || result.TotalRecordCount != 2 || result.StartIndex != 0 {
			t.Fatalf("status=%d result=%+v body=%q", response.Code, result, response.Body.String())
		}
		var wire map[string]json.RawMessage
		if json.Unmarshal(response.Body.Bytes(), &wire) != nil {
			t.Fatal("invalid local QueryResult JSON")
		}
		if _, exists := wire["StartIndex"]; exists {
			t.Fatalf("local QueryResult retained StartIndex: %s", response.Body.String())
		}
		if len(fake.requests) != 1 {
			t.Fatalf("metadata requests=%d, want one", len(fake.requests))
		}
		upstreamQuery := fake.requests[0].URL.Query()
		for _, key := range []string{"Filters", "IsFavorite", "UserData", "SortBy", "SortOrder", "api_key", "UserId"} {
			if upstreamQuery.Get(key) != "" {
				t.Fatalf("local %s escaped planned request: %v", key, upstreamQuery)
			}
		}
	})

	t.Run("resume same series", func(t *testing.T) {
		fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: `{"Items":[{"Id":"one","Type":"Episode","SeriesId":"series"},{"Id":"two","Type":"Episode","SeriesId":"series"}]}`, snapshot: personalPlanSourceUpstreamSnapshot()}
		server, store, _ := personalPlanHTTPServer(t, fake)
		defer server.Close()
		for i, id := range []string{"one", "two"} {
			if err := store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: id, SeriesID: "series", PlaybackPositionTicks: int64(i + 1)}); err != nil {
				t.Fatal(err)
			}
		}
		response := personalPlanHTTPRequest(server, "/Users/gateway-user/Items/Resume?api_key=gateway-token")
		var result struct{ Items []any }
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Items) != 2 {
			t.Fatalf("status=%d items=%d body=%q", response.Code, len(result.Items), response.Body.String())
		}
	})
}

func TestPersonalPlanHTTPNegativeTotalBeforeLimitZero(t *testing.T) {
	fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: `{"Items":[{"Id":"played"},{"Id":"unplayed"}]}`, snapshot: personalPlanSourceUpstreamSnapshot()}
	server, store, _ := personalPlanHTTPServer(t, fake)
	defer server.Close()
	if err := store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "played", Played: true}); err != nil {
		t.Fatal(err)
	}
	request := func(target string) *httptest.ResponseRecorder {
		return personalPlanHTTPRequest(server, target)
	}
	response := request("/Items?api_key=gateway-token&Filters=IsUnplayed&Limit=0")
	var result struct {
		Items            []any
		TotalRecordCount int
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Items) != 0 || result.TotalRecordCount != 1 {
		t.Fatalf("status=%d result=%+v body=%q", response.Code, result, response.Body.String())
	}
}

func TestPersonalPlanHTTPNextUpRouteAndBackendFailure(t *testing.T) {
	t.Run("one result", func(t *testing.T) {
		history := `{"Id":"episode-1","Type":"Episode","SeriesId":"series-1","SeasonId":"season-1","ParentIndexNumber":1,"IndexNumber":1}`
		next := `{"Id":"episode-2","Type":"Episode","SeriesId":"series-1","SeasonId":"season-1","ParentIndexNumber":1,"IndexNumber":2}`
		fake := &personalPlanSourceMetadataFake{responses: []personalPlanSourceMetadataResponse{
			{status: http.StatusOK, body: `{"Items":[` + history + `]}`, snapshot: personalPlanSourceUpstreamSnapshot()},
			{status: http.StatusOK, body: `{"Items":[` + history + `,` + next + `]}`, snapshot: personalPlanSourceUpstreamSnapshot()},
		}}
		server, store, _ := personalPlanHTTPServer(t, fake)
		defer server.Close()
		if err := store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "episode-1", SeriesID: "series-1", Played: true}); err != nil {
			t.Fatal(err)
		}
		response := personalPlanHTTPRequest(server, "/Shows/NextUp?api_key=gateway-token&Limit=1")
		var result struct{ Items []any }
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Items) != 1 {
			t.Fatalf("status=%d items=%d body=%q", response.Code, len(result.Items), response.Body.String())
		}
	})
	t.Run("backend failure", func(t *testing.T) {
		fake := &personalPlanSourceMetadataFake{err: context.DeadlineExceeded}
		server, store, _ := personalPlanHTTPServer(t, fake)
		defer server.Close()
		if err := store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "episode-1", SeriesID: "series-1", Played: true}); err != nil {
			t.Fatal(err)
		}
		response := personalPlanHTTPRequest(server, "/Shows/NextUp?api_key=gateway-token")
		if response.Code != http.StatusBadGateway || strings.HasPrefix(strings.TrimSpace(response.Body.String()), "{") {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
	})
}

func TestPersonalPlanHTTPLatestGroupedAndUngroupedArrays(t *testing.T) {
	t.Run("grouped default", func(t *testing.T) {
		fake := &personalPlanSourceMetadataFake{responses: []personalPlanSourceMetadataResponse{
			{status: http.StatusOK, body: `[{"Id":"episode-1","Type":"Episode","SeriesId":"series-1"}]`, snapshot: personalPlanSourceUpstreamSnapshot()},
			{status: http.StatusOK, body: `{"Items":[{"Id":"series-1","Type":"Series"}]}`, snapshot: personalPlanSourceUpstreamSnapshot()},
		}}
		server, _, _ := personalPlanHTTPServer(t, fake)
		defer server.Close()
		response := personalPlanHTTPRequest(server, "/Users/gateway-user/Items/Latest?api_key=gateway-token&Limit=1")
		var result []map[string]any
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result) != 1 || result[0]["Type"] != "Series" {
			t.Fatalf("status=%d result=%v body=%q", response.Code, result, response.Body.String())
		}
	})
	t.Run("ungrouped", func(t *testing.T) {
		fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: `[{"Id":"episode-1","Type":"Episode","SeriesId":"series-1"}]`, snapshot: personalPlanSourceUpstreamSnapshot()}
		server, _, _ := personalPlanHTTPServer(t, fake)
		defer server.Close()
		response := personalPlanHTTPRequest(server, "/Users/gateway-user/Items/Latest?api_key=gateway-token&GroupItems=false&Limit=1")
		var result []map[string]any
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result) != 1 || result[0]["Type"] != "Episode" {
			t.Fatalf("status=%d result=%v body=%q", response.Code, result, response.Body.String())
		}
	})
}

func TestPersonalPlanHTTPShowRefinementAndErrors(t *testing.T) {
	plan, err := preparePersonalHTTPPlan(personalRouteShowItems, "/Shows/show-1/Episodes", nil, &Session{SyntheticUserID: "gateway-user"})
	if err != nil || plan.Refinement.Get("SeriesId") != "show-1" || plan.Refinement.Get("IncludeItemTypes") != "Episode" {
		t.Fatalf("plan=%#v err=%v", plan, err)
	}
	if _, err := preparePersonalHTTPPlan(personalRouteShowItems, "/Shows/show-1/Episodes", urlValues("SeriesId", "other"), &Session{SyntheticUserID: "gateway-user"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("conflicting Show criteria error=%v", err)
	}

	fake := &personalPlanSourceMetadataFake{responses: []personalPlanSourceMetadataResponse{
		{status: http.StatusOK, body: `{"Items":[{"Id":"item-1","Type":"Episode","SeriesId":"other"}]}`, snapshot: personalPlanSourceUpstreamSnapshot()},
		{status: http.StatusOK, body: `{"Items":[]}`, snapshot: personalPlanSourceUpstreamSnapshot()},
	}}
	server, store, _ := personalPlanHTTPServer(t, fake)
	defer server.Close()
	if err := store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "item-1", IsFavorite: true}); err != nil {
		t.Fatal(err)
	}
	response := personalPlanHTTPRequest(server, "/Shows/show-1/Episodes?api_key=gateway-token&IsFavorite=true")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"TotalRecordCount":0`) {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestPersonalPlanHTTPStoreCandidateAndIncompleteErrors(t *testing.T) {
	t.Run("store snapshot", func(t *testing.T) {
		base := testStore("http://backend.invalid/emby")
		base.Sessions[HashToken("gateway-token")] = testSession()
		store := &personalPlanHTTPStore{MemoryStore: base, listErr: context.DeadlineExceeded}
		fake := &personalPlanSourceMetadataFake{}
		server, auth := personalPlanHTTPServerWithStore(t, store, base, fake)
		defer server.Close()
		response := personalPlanHTTPRequest(server, "/Items?api_key=gateway-token&IsFavorite=true")
		if response.Code != http.StatusInternalServerError || auth.ensure != 0 || fake.calls != 0 {
			t.Fatalf("status=%d ensure=%d metadata=%d", response.Code, auth.ensure, fake.calls)
		}
	})
	for _, test := range []struct {
		name string
		body string
	}{
		{"malformed candidate", `{"Items":[{"Name":"missing-id"}]}`},
		{"upstream status", `{"Items":[]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := http.StatusOK
			fake := &personalPlanSourceMetadataFake{status: status, body: test.body, snapshot: personalPlanSourceUpstreamSnapshot()}
			if test.name == "upstream status" {
				fake.status = http.StatusBadGateway
			}
			server, store, _ := personalPlanHTTPServer(t, fake)
			defer server.Close()
			if err := store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "item-1", IsFavorite: true}); err != nil {
				t.Fatal(err)
			}
			response := personalPlanHTTPRequest(server, "/Items?api_key=gateway-token&IsFavorite=true")
			if response.Code != http.StatusBadGateway {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
}

func TestPersonalPlanHTTPIncompleteAuditIsSafe(t *testing.T) {
	base := testStore("http://backend.invalid/emby")
	base.Sessions[HashToken("gateway-token")] = testSession()
	store := &personalPlanHTTPStore{MemoryStore: base}
	for i := 0; i < personalPlanScanMaxItems+1; i++ {
		if err := store.MemoryStore.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "item-" + strconv.Itoa(i), IsFavorite: true}); err != nil {
			t.Fatal(err)
		}
	}
	server, _ := personalPlanHTTPServerWithStore(t, store, base, &personalPlanSourceMetadataFake{})
	defer server.Close()
	response := personalPlanHTTPRequest(server, "/Items?api_key=gateway-token&IsFavorite=true&SearchTerm=secret")
	if response.Code != http.StatusServiceUnavailable || strings.HasPrefix(strings.TrimSpace(response.Body.String()), "{") || len(store.AuditLogs) != 1 {
		t.Fatalf("status=%d audits=%d body=%q", response.Code, len(store.AuditLogs), response.Body.String())
	}
	audit := store.AuditLogs[0]
	if audit.Event != "personal_query_incomplete" || audit.ErrorKind != "personal_query_incomplete" || audit.Status != http.StatusServiceUnavailable || audit.GatewayUserID != "u1" || audit.SyntheticUserID != "gateway-user" || audit.Path != "/Items" || strings.Contains(audit.Message, "secret") || strings.Contains(audit.Message, "gateway-token") || strings.Contains(audit.Message, "item-") {
		t.Fatalf("unsafe or incorrect audit=%#v", audit)
	}
}

func TestPersonalPlanHTTPUnsafeNegativeCandidateIsUpstreamFailure(t *testing.T) {
	base := testStore("http://backend.invalid/emby")
	base.Sessions[HashToken("gateway-token")] = testSession()
	store := &personalPlanHTTPStore{MemoryStore: base}
	fake := &personalPlanSourceMetadataFake{
		status:   http.StatusOK,
		body:     `{"Items":[{"Id":"bad/id"}]}`,
		snapshot: personalPlanSourceUpstreamSnapshot(),
	}
	server, _ := personalPlanHTTPServerWithStore(t, store, base, fake)
	defer server.Close()

	response := personalPlanHTTPRequest(server, "/Items?api_key=gateway-token&Filters=IsUnplayed")
	body := response.Body.String()
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502; body=%q", response.Code, body)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control=%q, want no-store", response.Header().Get("Cache-Control"))
	}
	if strings.HasPrefix(strings.TrimSpace(body), "{") || strings.Contains(body, "500") || strings.Contains(strings.ToLower(body), "internal") || strings.Contains(body, "bad/id") {
		t.Fatalf("unsafe or non-generic failure body: %q", body)
	}
	if store.batchCalls != 0 || store.listCalls != 1 {
		t.Fatalf("store calls list=%d batch-overlay=%d, want one snapshot and no overlay", store.listCalls, store.batchCalls)
	}
	for _, audit := range store.AuditLogs {
		if strings.Contains(audit.Message, "bad/id") || strings.Contains(audit.Message, "500") || strings.Contains(strings.ToLower(audit.Message), "internal") {
			t.Fatalf("unsafe audit output: %#v", audit)
		}
	}
}

func urlValues(key, value string) url.Values { return url.Values{key: []string{value}} }
