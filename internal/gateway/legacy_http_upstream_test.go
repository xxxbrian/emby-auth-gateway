package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLegacyHTTPUpstreamReportsRefreshResultAndClosesDiscardedUnauthorizedOnce(t *testing.T) {
	refreshFailure := errors.New("refresh failed")
	for _, tt := range []struct {
		name       string
		refreshErr error
		wantCalls  int
	}{
		{name: "success", wantCalls: 2},
		{name: "failure", refreshErr: refreshFailure, wantCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls int
			var results []upstreamRefreshResult
			unauthorizedBody := &adapterCloseCountingBody{Reader: strings.NewReader("unauthorized")}
			finalBody := &adapterCloseCountingBody{Reader: strings.NewReader("ok")}
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls == 2 && len(results) != 1 {
					t.Fatal("retry started before refresh result notification")
				}
				status, body := http.StatusUnauthorized, io.ReadCloser(unauthorizedBody)
				if calls == 2 {
					status, body = http.StatusOK, finalBody
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: body, Request: req}, nil
			})}
			first := testUpstreamSnapshot("http://backend.invalid")
			second := first
			second.token = "refreshed-token"
			adapter := newLegacyHTTPUpstream(client, nil, func(context.Context, upstreamRequestSnapshot) (upstreamRequestSnapshot, bool, error) {
				return second, true, tt.refreshErr
			}, nil)
			resp, err := adapter.RoundTripLegacy(legacyUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{
				Request: httptest.NewRequest(http.MethodGet, "http://gateway.test/Unknown", nil),
				Session: &Session{SyntheticUserID: "gateway-user"}, Snapshot: first,
				refreshResult: func(result upstreamRefreshResult) { results = append(results, result) },
			}})
			if err != nil {
				t.Fatal(err)
			}
			if calls != tt.wantCalls || len(results) != 1 || !results[0].Confirmed || !errors.Is(results[0].Err, tt.refreshErr) {
				t.Fatalf("calls=%d results=%+v", calls, results)
			}
			if tt.refreshErr == nil && unauthorizedBody.closes != 1 {
				t.Fatalf("discarded unauthorized closes=%d, want 1", unauthorizedBody.closes)
			}
			_ = resp.Body.Close()
			_ = resp.Body.Close()
			if tt.refreshErr == nil && finalBody.closes != 1 {
				t.Fatalf("final response closes=%d, want 1", finalBody.closes)
			}
			if tt.refreshErr != nil && unauthorizedBody.closes != 1 {
				t.Fatalf("returned unauthorized closes=%d, want 1", unauthorizedBody.closes)
			}
		})
	}
}
