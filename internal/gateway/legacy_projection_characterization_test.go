package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCharacterizationLegacyRoundTripOwnsBroadRequestRewritingAndManagedCredentials(t *testing.T) {
	var got *http.Request
	var gotBody string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		got = req.Clone(req.Context())
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		gotBody = string(body)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: req}, nil
	})}
	snapshot := testUpstreamSnapshot("http://backend.invalid/emby")
	session := &Session{SyntheticUserID: "gateway-user"}
	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/Unknown/prefix-gateway-user-suffix?note=prefix-gateway-user-suffix&selected=gateway-token&access_token=client-secret", strings.NewReader(`{"UserId":"gateway-user","Note":"prefix-gateway-user-suffix gateway-token-suffix"}`))
	req.Header.Set("Authorization", "Bearer gateway-token")
	req.Header.Set("Cookie", "session=gateway-token")
	req.Header.Set("X-Emby-Token", "gateway-token")
	req.Header.Set("X-Emby-Authorization", `Emby Token="gateway-token"`)

	resp, err := newLegacyHTTPUpstream(client, nil, nil, nil).RoundTripLegacy(legacyUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{
		Request:  req,
		Session:  session,
		Snapshot: snapshot,
	}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if got == nil {
		t.Fatal("legacy transport was not called")
	}
	if got.URL.Path != "/emby/Unknown/prefix-backend-user-suffix" {
		t.Fatalf("legacy path = %q", got.URL.Path)
	}
	query := got.URL.Query()
	if query.Get("note") != "prefix-backend-user-suffix" || query.Get("selected") != "" || query.Get("access_token") != "" || query.Get("api_key") != "backend-token" {
		t.Fatalf("legacy query = %v", query)
	}
	if gotBody != `{"UserId":"backend-user","Note":"prefix-backend-user-suffix backend-token-suffix"}` {
		t.Fatalf("legacy body = %q", gotBody)
	}
	if got.Header.Get("Authorization") != "" || got.Header.Get("Cookie") != "" || got.Header.Get("X-Emby-Token") != "backend-token" {
		t.Fatalf("legacy managed headers = %#v", got.Header)
	}
	auth := ParseEmbyAuthHeader(got.Header.Get("X-Emby-Authorization"))
	if auth.Token != "backend-token" || auth.UserID != "backend-user" {
		t.Fatalf("legacy managed authorization = %#v", auth)
	}
}

func TestCharacterizationPurposeBoundNegotiationDoesNotBroadlyRewriteArbitraryBodyStrings(t *testing.T) {
	var gotBody map[string]any
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: req}, nil
	})}
	snapshot := testUpstreamSnapshot("http://backend.invalid/emby")
	session := &Session{GatewayTokenHash: "owner", SyntheticUserID: "gateway-user"}
	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/Items/item/PlaybackInfo?note=prefix-gateway-user-suffix", strings.NewReader(`{"UserId":"gateway-user","Note":"prefix-gateway-user-suffix gateway-token-suffix"}`))
	req.Header.Set("X-Emby-Token", "gateway-token")

	result, err := newMediaUpstream(client, nil, nil, nil).RoundTripNegotiation(negotiationUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{
		Request:  req,
		Session:  session,
		Snapshot: snapshot,
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Registration.Close()
	_ = result.Response.Body.Close()

	if gotBody["UserId"] != "backend-user" {
		t.Fatalf("purpose-bound UserId = %#v", gotBody["UserId"])
	}
	if gotBody["Note"] != "prefix-gateway-user-suffix gateway-token-suffix" {
		t.Fatalf("purpose-bound arbitrary string was broadly rewritten: %#v", gotBody["Note"])
	}
}

func TestCharacterizationLegacyRefreshRetriesWithRefreshedManagedIdentity(t *testing.T) {
	var requests []*http.Request
	var bodies []string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Clone(req.Context()))
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, string(body))
		status := http.StatusUnauthorized
		if len(requests) == 2 {
			status = http.StatusOK
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(http.StatusText(status))), Request: req}, nil
	})}
	first := testUpstreamSnapshot("http://first.invalid/emby")
	refreshed := first
	refreshed.baseURL = "http://refreshed.invalid/emby"
	refreshed.userID = "refreshed-user"
	refreshed.token = "refreshed-token"
	refreshed.identity.DeviceID = "refreshed-device"
	refreshedRef := first
	var refreshResults []upstreamRefreshResult
	adapter := newLegacyHTTPUpstream(client, nil, func(_ context.Context, got upstreamRequestSnapshot) (upstreamRequestSnapshot, bool, error) {
		if got.token != first.token {
			t.Fatalf("refresh snapshot token = %q", got.token)
		}
		return refreshed, true, nil
	}, nil)
	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/Unknown/gateway-user", strings.NewReader(`{"UserId":"gateway-user","Token":"gateway-token"}`))
	req.Header.Set("X-Emby-Token", "gateway-token")

	resp, err := adapter.RoundTripLegacy(legacyUpstreamRequest{
		upstreamHTTPRequest: upstreamHTTPRequest{
			Request:       req,
			Session:       &Session{SyntheticUserID: "gateway-user"},
			Snapshot:      first,
			refreshResult: func(result upstreamRefreshResult) { refreshResults = append(refreshResults, result) },
		},
		SnapshotRef: &refreshedRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if len(requests) != 2 || len(refreshResults) != 1 || !refreshResults[0].Confirmed || refreshResults[0].Err != nil {
		t.Fatalf("requests=%d refresh results=%+v", len(requests), refreshResults)
	}
	if requests[0].URL.Host != "first.invalid" || requests[1].URL.Host != "refreshed.invalid" {
		t.Fatalf("retry hosts = %q, %q", requests[0].URL.Host, requests[1].URL.Host)
	}
	if requests[0].Header.Get("X-Emby-Token") != "backend-token" || requests[1].Header.Get("X-Emby-Token") != "refreshed-token" {
		t.Fatalf("retry tokens = %q, %q", requests[0].Header.Get("X-Emby-Token"), requests[1].Header.Get("X-Emby-Token"))
	}
	if bodies[0] != `{"UserId":"backend-user","Token":"backend-token"}` || bodies[1] != `{"UserId":"refreshed-user","Token":"refreshed-token"}` {
		t.Fatalf("retry bodies = %q, %q", bodies[0], bodies[1])
	}
	if refreshedRef.baseURL != refreshed.baseURL || refreshedRef.userID != refreshed.userID || refreshedRef.token != refreshed.token || refreshedRef.identity.DeviceID != refreshed.identity.DeviceID {
		t.Fatalf("snapshot reference = %#v", refreshedRef)
	}
}

func TestCharacterizationLegacyResponseProjectionRewritesBodyAndStructuralLocation(t *testing.T) {
	snapshot := testUpstreamSnapshot("http://backend.invalid/emby")
	snapshot.serverID = "backend-server"
	session := &Session{SyntheticUserID: "gateway-user"}
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Unknown", nil)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":   []string{"application/json"},
			"Content-Length": []string{"999"},
			"X-Legacy-Debt":  []string{"prefix-backend-user-backend-server-suffix"},
			"Location":       []string{"http://backend.invalid/emby/Unknown/backend-user?api_key=backend-token"},
		},
		Body:    io.NopCloser(strings.NewReader(`{"Note":"prefix-backend-token-backend-user-backend-server-suffix"}`)),
		Request: request,
	}
	server := NewServer(Config{GatewayServerID: "gateway-server"}, NewMemoryStore())
	defer server.Close()
	writer := httptest.NewRecorder()

	server.writeProxyResponseWithSnapshot(writer, request, "/Unknown", response, session, snapshot, "gateway-token", "https://gateway.test/emby")

	var body map[string]any
	if err := json.Unmarshal(writer.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["Note"] != "prefix-gateway-token-gateway-user-gateway-server-suffix" {
		t.Fatalf("legacy projected body = %#v", body)
	}
	if got := writer.Header().Get("X-Legacy-Debt"); got != "prefix-gateway-user-gateway-server-suffix" {
		t.Fatalf("legacy safe arbitrary header = %q", got)
	}
	if got := writer.Header().Get("Location"); got != "https://gateway.test/emby/Unknown/backend-user" {
		t.Fatalf("legacy projected location = %q", got)
	}
	if got := writer.Header().Get("Content-Length"); got != "" {
		t.Fatalf("legacy projected content length = %q", got)
	}
}

// Remove with the Legacy compatibility path in Phase 8.
func TestCharacterizationLegacyResponseCredentialHeaderFailsClosed(t *testing.T) {
	snapshot := testUpstreamSnapshot("http://backend.invalid/emby")
	snapshot.serverID = "backend-server"
	session := &Session{SyntheticUserID: "gateway-user"}
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Unknown", nil)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":  []string{"application/json"},
			"X-Legacy-Debt": []string{"prefix-backend-token-suffix"},
			"Location":      []string{"http://backend.invalid/emby/Unknown?api_key=backend-token"},
		},
		Body:    io.NopCloser(strings.NewReader(`{"Note":"backend-token"}`)),
		Request: request,
	}
	server := NewServer(Config{GatewayServerID: "gateway-server"}, NewMemoryStore())
	defer server.Close()
	writer := httptest.NewRecorder()

	server.writeProxyResponseWithSnapshot(writer, request, "/Unknown", response, session, snapshot, "gateway-token", "https://gateway.test/emby")

	if writer.Code != http.StatusBadGateway || writer.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("legacy credential response = %d headers=%v body=%q", writer.Code, writer.Header(), writer.Body.String())
	}
	for _, name := range []string{"X-Legacy-Debt", "Location", "Content-Location"} {
		if writer.Header().Get(name) != "" {
			t.Fatalf("unsafe legacy header %s survived: %v", name, writer.Header())
		}
	}
	if strings.Contains(writer.Body.String(), "backend-token") || strings.Contains(writer.Body.String(), "backend.invalid") {
		t.Fatalf("unsafe legacy failure body leaked backend data: %q", writer.Body.String())
	}
}
