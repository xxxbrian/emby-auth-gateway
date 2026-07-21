package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamingUpstreamsClearClientTimeoutAndPreserveCustomTransport(t *testing.T) {
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if _, ok := req.Context().Deadline(); ok {
			t.Fatal("streaming request inherited a total deadline")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("stream")), ContentLength: 6, Request: req}, nil
	})
	base := &http.Client{Transport: transport, Timeout: time.Nanosecond}
	media := newMediaUpstream(base, nil, nil, nil)
	_, mediaCustom := media.client.Transport.(roundTripFunc)
	if media.client.Timeout != 0 || !mediaCustom {
		t.Fatalf("media client = %#v", media.client)
	}
	snapshot := testUpstreamSnapshot("http://backend.invalid")
	mediaResp, err := media.RoundTripMedia(mediaUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: httptest.NewRequest(http.MethodGet, "http://gateway.test/Videos/item/stream", nil), Session: &Session{SyntheticUserID: "gateway-user"}, Snapshot: snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	defer mediaResp.Body.Close()
	if body, err := io.ReadAll(mediaResp.Body); err != nil || string(body) != "stream" {
		t.Fatalf("media body=%q err=%v", body, err)
	}
}

func TestMediaUpstreamUsesManagedHeadersAndRetriesUnauthorized(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if (r.Header.Get("X-Emby-Token") != "backend-token" && r.Header.Get("X-Emby-Token") != "refreshed-token") || r.Header.Get("User-Agent") != "managed-agent" {
			t.Fatalf("managed headers missing: %#v", r.Header)
		}
		if values := r.URL.Query()["api_key"]; len(values) != 1 || values[0] != r.Header.Get("X-Emby-Token") || r.URL.Query().Get("signature") != "cdn-signature" {
			t.Fatalf("managed query missing: %v", r.URL.Query())
		}
		for key := range r.URL.Query() {
			if isEgressCredentialAliasQueryKey(key) && key != "api_key" {
				t.Fatalf("client credential alias reached backend: %q=%v", key, r.URL.Query()[key])
			}
		}
		for _, name := range []string{"Connection", "Keep-Alive", "X-Hop", "Proxy-Authorization"} {
			if r.Header.Get(name) != "" {
				t.Fatalf("hop header %s reached backend: %#v", name, r.Header)
			}
		}
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("stream"))
	}))
	defer server.Close()
	refreshed := testUpstreamSnapshot(server.URL)
	refreshed.token = "refreshed-token"
	refreshed.identity.UserAgent = "managed-agent"
	adapter := newMediaUpstream(server.Client(), func(_ context.Context, _ upstreamRequestSnapshot) (upstreamRequestSnapshot, bool, error) {
		return refreshed, true, nil
	}, nil, nil)
	snapshot := testUpstreamSnapshot(server.URL)
	snapshot.identity.UserAgent = "managed-agent"
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/Videos/item/stream?API_KEY=upper&TOKEN=generic&x-mediabrowser-token=media&api_key=client-token&signature=cdn-signature", nil)
	req.Header.Set("Range", "bytes=10-")
	req.Header.Add("Connection", "X-Emby-Token, X-Hop")
	req.Header.Add("Connection", "Keep-Alive")
	req.Header.Set("X-Hop", "remove")
	req.Header.Set("Proxy-Authorization", "remove")
	resp, err := adapter.RoundTripMedia(mediaUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: &Session{SyntheticUserID: "gateway-user"}, Snapshot: snapshot}})
	if err != nil || resp == nil || resp.StatusCode != http.StatusPartialContent || calls != 2 {
		t.Fatalf("response=%v err=%v calls=%d", resp, err, calls)
	}
	defer resp.Body.Close()
}

func TestMediaUpstreamReportsRefreshResultBeforeRetryAndClosesDiscardedUnauthorizedOnce(t *testing.T) {
	var calls int
	var results []upstreamRefreshResult
	unauthorizedBody := &adapterCloseCountingBody{Reader: strings.NewReader("unauthorized")}
	finalBody := &adapterCloseCountingBody{Reader: strings.NewReader("stream")}
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
	adapter := newMediaUpstream(client, func(context.Context, upstreamRequestSnapshot) (upstreamRequestSnapshot, bool, error) {
		return second, true, nil
	}, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/Videos/item/stream", nil)
	resp, err := adapter.RoundTripMedia(mediaUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{
		Request: req, Session: &Session{SyntheticUserID: "gateway-user"}, Snapshot: first,
		refreshResult: func(result upstreamRefreshResult) { results = append(results, result) },
	}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(results) != 1 || !results[0].Confirmed || results[0].Err != nil {
		t.Fatalf("calls=%d results=%+v", calls, results)
	}
	if unauthorizedBody.closes != 1 {
		t.Fatalf("discarded unauthorized closes=%d, want 1", unauthorizedBody.closes)
	}
	_ = resp.Body.Close()
	_ = resp.Body.Close()
	if finalBody.closes != 1 {
		t.Fatalf("final response closes=%d, want 1", finalBody.closes)
	}
}

func TestNegotiationUpstreamReportsRefreshFailure(t *testing.T) {
	refreshErr := errors.New("refresh failed")
	var results []upstreamRefreshResult
	body := &adapterCloseCountingBody{Reader: strings.NewReader("unauthorized")}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: body, Request: req}, nil
	})}
	adapter := newMediaUpstream(client, func(context.Context, upstreamRequestSnapshot) (upstreamRequestSnapshot, bool, error) {
		return upstreamRequestSnapshot{}, true, refreshErr
	}, NewMediaLeaseRegistry(nil), nil)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/Items/item/PlaybackInfo", nil)
	result, err := adapter.RoundTripNegotiation(negotiationUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{
		Request: req, Session: &Session{GatewayTokenHash: "owner", SyntheticUserID: "gateway-user"}, Snapshot: testUpstreamSnapshot("http://backend.invalid"),
		refreshResult: func(result upstreamRefreshResult) { results = append(results, result) },
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Confirmed || !errors.Is(results[0].Err, refreshErr) {
		t.Fatalf("results=%+v", results)
	}
	defer result.Registration.Close()
	_ = result.Response.Body.Close()
	_ = result.Response.Body.Close()
	if body.closes != 1 {
		t.Fatalf("returned unauthorized closes=%d, want 1", body.closes)
	}
}

func TestNegotiationStructuredRewriteAndLeaseRegistration(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"PlaySessionId":"play-1","LiveStreamId":"live-1"}`))
	}))
	defer server.Close()
	registry := NewMediaLeaseRegistry(nil)
	adapter := newMediaUpstream(server.Client(), nil, registry, nil)
	snapshot := testUpstreamSnapshot(server.URL)
	session := &Session{GatewayTokenHash: "owner", SyntheticUserID: "gateway-user"}
	deviceProfile := `{"CodecProfiles":[{"Type":"Video","Conditions":[{"Property":"VideoCodec","Value":"h264+hevc"}]}],"Unknown":{"raw":"a+b"}}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/LiveStreams/Open", strings.NewReader(`{"UserId":"gateway-user","DeviceId":"client-device","SessionId":"secret","ControllingUserId":"other","DeviceProfile":`+deviceProfile+`}`))
	result, err := adapter.RoundTripNegotiation(negotiationUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: session, Snapshot: snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Response.Body.Close()
	defer result.Registration.Close()
	for _, secret := range []string{"gateway-user", "client-device", "\"SessionId\"", "\"ControllingUserId\""} {
		if strings.Contains(gotBody, secret) {
			t.Fatalf("rewrite leaked %q: %s", secret, gotBody)
		}
	}
	if !strings.Contains(gotBody, `"DeviceProfile":`+deviceProfile) {
		t.Fatalf("DeviceProfile changed: %s", gotBody)
	}
	if _, err := registry.Validate("owner", "play-1", "live-1", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lease registered before commit: %v", err)
	}
	if err := result.Registration.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := result.Registration.Commit(); err != nil {
		t.Fatalf("idempotent commit: %v", err)
	}
	if _, err := registry.Validate("owner", "play-1", "live-1", time.Time{}); err != nil {
		t.Fatalf("registered lease unavailable: %v", err)
	}
}

func TestPlaybackInfoNullableSelectorsAreAbsentAndValidIDsRegister(t *testing.T) {
	responses := []string{
		`{"MediaSources":[{"LiveStreamId":null}],"PlaySessionId":"play-valid"}`,
		`{"MediaSources":[{"LiveStreamId":null}],"PlaySessionId":""}`,
	}
	registry := NewMediaLeaseRegistry(nil)
	var calls int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responses[calls])
		calls++
	}))
	defer backend.Close()
	adapter := newMediaUpstream(backend.Client(), nil, registry, nil)
	session := &Session{GatewayTokenHash: "owner", SyntheticUserID: "gateway-user"}
	for _, body := range []string{
		`{"DeviceProfile":{},"LiveStreamId":null,"PlaySessionId":""}`,
		`{"DeviceProfile":{},"Nested":{"LiveStreamId":null}}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "http://gateway.test/Items/item/PlaybackInfo?LiveStreamId=&playsessionid=", strings.NewReader(body))
		result, err := adapter.RoundTripNegotiation(negotiationUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: session, Snapshot: testUpstreamSnapshot(backend.URL)}})
		if err != nil {
			t.Fatal(err)
		}
		if result.Registration != nil {
			defer result.Registration.Close()
			if err := result.Registration.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		_ = result.Response.Body.Close()
	}
	if err := registry.ValidateAll("owner", []PlaySessionID{"play-valid"}, nil, time.Time{}); err != nil {
		t.Fatalf("valid sibling identifier was not registered: %v", err)
	}
	concrete := registry.(*mediaLeaseRegistry)
	concrete.mu.Lock()
	leaseCount := len(concrete.leases)
	concrete.mu.Unlock()
	if leaseCount != 1 {
		t.Fatalf("null-only response registered a lease: count=%d", leaseCount)
	}
}

func TestNegotiationValidatesEveryBodyAndQueryIdentifierBeforeDial(t *testing.T) {
	registry := NewMediaLeaseRegistry(nil)
	if err := registry.RegisterAll("owner", []PlaySessionID{"owned-play"}, []LiveStreamID{"owned-live"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(MediaLease{GatewayTokenHash: "foreign", LiveStreamID: "foreign-live"}); err != nil {
		t.Fatal(err)
	}
	var hits int
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer backend.Close()
	adapter := newMediaUpstream(backend.Client(), nil, registry, nil)
	body := `{"playSESSIONid":"owned-play","nested":{"LiveStreamId":"owned-live","LiveStreamId":"foreign-live"}}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/LiveStreams/Close?PlaySessionId=owned-play&playsessionid=unknown-play", strings.NewReader(body))
	_, err := adapter.RoundTripNegotiation(negotiationUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: &Session{GatewayTokenHash: "owner", SyntheticUserID: "gateway-user"}, Snapshot: testUpstreamSnapshot(backend.URL)}})
	if !errors.Is(err, ErrNotFound) || hits != 0 {
		t.Fatalf("err=%v hits=%d", err, hits)
	}
}

func TestNegotiationRejectsMalformedIdentifierAliases(t *testing.T) {
	adapter := newMediaUpstream(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("malformed identifiers dialed upstream")
		return nil, nil
	})}, nil, NewMediaLeaseRegistry(nil), nil)
	for _, target := range []string{
		"http://gateway.test/LiveStreams/Close?PlaySessionId=",
		"http://gateway.test/LiveStreams/Close?PLAYSESSIONID=ok",
	} {
		body := `{"LiveStreamId":123}`
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
		if _, err := adapter.RoundTripNegotiation(negotiationUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: &Session{GatewayTokenHash: "owner", SyntheticUserID: "gateway-user"}, Snapshot: testUpstreamSnapshot("http://backend.invalid")}}); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("target=%s err=%v", target, err)
		}
	}
}

func TestNegotiationRetryRebuildsCaseInsensitiveAliasesWithFreshSnapshot(t *testing.T) {
	var bodies [][]byte
	var calls int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer backend.Close()
	first := testUpstreamSnapshot(backend.URL)
	first.userID = "old-user"
	first.identity.DeviceID = "old-device"
	second := first
	second.userID = "new-user"
	second.token = "new-token"
	second.identity.DeviceID = "new-device"
	adapter := newMediaUpstream(backend.Client(), func(context.Context, upstreamRequestSnapshot) (upstreamRequestSnapshot, bool, error) {
		return second, true, nil
	}, NewMediaLeaseRegistry(nil), nil)
	body := `{"userID":"client-user","DEVICEid":"client-device","sessionID":"secret","CONTROLLINGuserID":"secret","Nested":{"opaque":"old-user","device":"old-device"}}`
	result, err := adapter.RoundTripNegotiation(negotiationUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: httptest.NewRequest(http.MethodPost, "http://gateway.test/LiveStreams/Open", strings.NewReader(body)), Session: &Session{GatewayTokenHash: "owner", SyntheticUserID: "gateway-user"}, Snapshot: first}})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Response.Body.Close()
	defer result.Registration.Close()
	if len(bodies) != 2 || bytes.Contains(bodies[1], []byte(`"UserId":"old-user"`)) || bytes.Contains(bodies[1], []byte(`"DeviceId":"old-device"`)) || bytes.Contains(bytes.ToLower(bodies[1]), []byte("sessionid")) || bytes.Contains(bytes.ToLower(bodies[1]), []byte("controllinguserid")) {
		t.Fatalf("retry bodies=%s", bodies)
	}
	if !bytes.Contains(bodies[0], []byte(`"UserId":"old-user"`)) || !bytes.Contains(bodies[0], []byte(`"DeviceId":"old-device"`)) {
		t.Fatalf("first attempt did not use original document with initial snapshot: %s", bodies[0])
	}
	if strings.Count(string(bodies[1]), `"UserId":"new-user"`) != 1 || strings.Count(string(bodies[1]), `"DeviceId":"new-device"`) != 1 || !bytes.Contains(bodies[1], []byte(`"Nested":{"opaque":"old-user","device":"old-device"}`)) {
		t.Fatalf("fresh canonical rewrite=%s", bodies[1])
	}
}

func TestNegotiationRejectsAmbiguousIdentityBeforeDial(t *testing.T) {
	var hits int
	adapter := newMediaUpstream(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		hits++
		return nil, nil
	})}, nil, NewMediaLeaseRegistry(nil), nil)
	for _, body := range []string{
		`{"UserId":"one","userID":"two"}`,
		`{"DeviceProfile":{"UserId":"nested"}}`,
		`{"Unknown":[{"DEVICEid":"nested"}]}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "http://gateway.test/LiveStreams/Open", strings.NewReader(body))
		_, err := adapter.RoundTripNegotiation(negotiationUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: &Session{GatewayTokenHash: "owner", SyntheticUserID: "gateway-user"}, Snapshot: testUpstreamSnapshot("http://backend.invalid")}})
		if !errors.Is(err, ErrBadRequest) {
			t.Fatalf("body=%s err=%v", body, err)
		}
	}
	if hits != 0 {
		t.Fatalf("ambiguous bodies dialed upstream %d times", hits)
	}
}

func TestMediaUpstreamPreservesRawQueryAndPathSubstring(t *testing.T) {
	var requestURI string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURI = r.RequestURI
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	// Exact admitted binary image path; item id contains synthetic-user substring.
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/Items/gateway-user-copy/Images/Primary?sig=a%2Bb&dup=one&dup=two+words&opaque=gateway-user&UserId=gateway-user", nil)
	resp, err := newMediaUpstream(backend.Client(), nil, nil, nil).RoundTripMedia(mediaUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: &Session{SyntheticUserID: "gateway-user"}, Snapshot: testUpstreamSnapshot(backend.URL)}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	want := "/Items/gateway-user-copy/Images/Primary?sig=a%2Bb&dup=one&dup=two+words&opaque=gateway-user&UserId=backend-user&api_key=backend-token"
	if requestURI != want {
		t.Fatalf("request URI=%q, want %q", requestURI, want)
	}
}

func TestMediaUpstreamAcceptsWrappedZeroLengthBodyGETAndHEAD(t *testing.T) {
	var dials atomic.Int32
	var methods []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dials.Add(1)
		methods = append(methods, r.Method)
		if r.URL.Path != "/Items/item/Images/Primary" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "image/gif")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("image"))
		}
	}))
	defer backend.Close()
	adapter := newMediaUpstream(backend.Client(), nil, nil, nil)
	snapshot := testUpstreamSnapshot(backend.URL)
	session := &Session{SyntheticUserID: "gateway-user", GatewayTokenHash: "owner"}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequest(method, "http://gateway.test/Items/item/Images/Primary?maxHeight=360&maxWidth=640&tag=abc&quality=90", nil)
		// PocketBase *router.RereadableReadCloser around an empty body.
		req.Body = io.NopCloser(strings.NewReader(""))
		req.ContentLength = 0
		resp, err := adapter.RoundTripMedia(mediaUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: session, Snapshot: snapshot}})
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", method, resp.StatusCode)
		}
	}
	if dials.Load() != 2 || len(methods) != 2 || methods[0] != http.MethodGet || methods[1] != http.MethodHead {
		t.Fatalf("dials=%d methods=%v", dials.Load(), methods)
	}
}

func TestMediaUpstreamRejectsDeclaredBodiesBeforeDial(t *testing.T) {
	var dials atomic.Int32
	adapter := newMediaUpstream(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		dials.Add(1)
		return nil, nil
	})}, nil, nil, nil)
	snapshot := testUpstreamSnapshot("http://backend.invalid")
	session := &Session{SyntheticUserID: "gateway-user", GatewayTokenHash: "owner"}
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
			req := httptest.NewRequest(http.MethodGet, "http://gateway.test/Items/item/Images/Primary", nil)
			tt.edit(req)
			_, err := adapter.RoundTripMedia(mediaUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: session, Snapshot: snapshot}})
			if !errors.Is(err, ErrMediaRequestRejected) {
				t.Fatalf("error = %v, want ErrMediaRequestRejected", err)
			}
		})
	}
	if dials.Load() != 0 {
		t.Fatalf("dials = %d, want 0", dials.Load())
	}
}

func TestPlaybackInfoLiveStreamRegistrationFailureClosesAutoOpen(t *testing.T) {
	registry := NewMediaLeaseRegistry(nil)
	for i := 0; i < mediaLeaseRegistryMaxPerToken; i++ {
		if err := registry.Register(MediaLease{GatewayTokenHash: "owner", PlaySessionID: PlaySessionID(fmt.Sprintf("full-%d", i))}); err != nil {
			t.Fatal(err)
		}
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"PlaySessionId":"auto-play","LiveStreamId":"auto-live"}`)
	}))
	defer backend.Close()
	adapter := newMediaUpstream(backend.Client(), nil, registry, nil)
	var closed []LiveStreamID
	adapter.closeStream = func(_ context.Context, _ upstreamRequestSnapshot, _ PlaySessionID, live LiveStreamID) error {
		closed = append(closed, live)
		return nil
	}
	result, err := adapter.RoundTripNegotiation(negotiationUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: httptest.NewRequest(http.MethodGet, "http://gateway.test/Items/item/PlaybackInfo", nil), Session: &Session{GatewayTokenHash: "owner", SyntheticUserID: "gateway-user"}, Snapshot: testUpstreamSnapshot(backend.URL)}})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Response.Body.Close()
	err = result.Registration.Commit()
	if !errors.Is(err, ErrStoreUnavailable) || len(closed) != 1 || closed[0] != "auto-live" {
		t.Fatalf("err=%v closed=%v", err, closed)
	}
	result.Registration.Close()
	if len(closed) != 1 {
		t.Fatalf("idempotent close count=%d", len(closed))
	}
}

func TestSuccessfulCloseReleasesAllSuppliedIdentifiers(t *testing.T) {
	registry := NewMediaLeaseRegistry(nil)
	if err := registry.RegisterAll("owner", []PlaySessionID{"play-a", "play-b"}, []LiveStreamID{"live-a"}); err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer backend.Close()
	adapter := newMediaUpstream(backend.Client(), nil, registry, nil)
	body := `{"PLAYSESSIONID":"play-b","Nested":{"livestreamid":"live-a"}}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/LiveStreams/Close?PlaySessionId=play-a", strings.NewReader(body))
	result, err := adapter.RoundTripNegotiation(negotiationUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: &Session{GatewayTokenHash: "owner", SyntheticUserID: "gateway-user"}, Snapshot: testUpstreamSnapshot(backend.URL)}})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Response.Body.Close()
	if result.Registration != nil {
		t.Fatal("close operation returned pending registration")
	}
	if err := registry.ValidateAll("owner", []PlaySessionID{"play-a"}, nil, time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("released identifiers remain: %v", err)
	}
}

func TestMediaUpstreamRejectsForeignLeaseBeforeDial(t *testing.T) {
	var hits int
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer backend.Close()
	registry := NewMediaLeaseRegistry(nil)
	if err := registry.Register(MediaLease{GatewayTokenHash: "owner-a", PlaySessionID: "play-a"}); err != nil {
		t.Fatal(err)
	}
	adapter := newMediaUpstream(backend.Client(), nil, registry, nil)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/Videos/item/stream?PlaySessionId=play-a", nil)
	_, err := adapter.RoundTripMedia(mediaUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: &Session{GatewayTokenHash: "owner-b", SyntheticUserID: "gateway-user"}, Snapshot: testUpstreamSnapshot(backend.URL)}})
	if !errors.Is(err, ErrNotFound) || hits != 0 {
		t.Fatalf("err=%v hits=%d", err, hits)
	}
}

func TestSanitizeHopHeadersAndResponseCopy(t *testing.T) {
	header := make(http.Header)
	header.Add("Connection", "X-Hop, x-second")
	header.Add("Connection", "Keep-Alive")
	header.Set("X-Hop", "remove")
	header.Set("X-Second", "remove")
	header.Set("Proxy-Connection", "remove")
	header.Set("Proxy-Authenticate", "remove")
	header.Set("Proxy-Authorization", "remove")
	header.Set("TE", "trailers")
	header.Set("Trailer", "X-Trailer")
	header.Set("Transfer-Encoding", "chunked")
	header.Set("Upgrade", "websocket")
	header.Set("Content-Type", "video/mp4")
	header.Set("Content-Range", "bytes 0-9/10")
	header.Set("Cache-Control", "public, max-age=60")
	sanitizeHopHeaders(header)
	for _, name := range []string{"Connection", "X-Hop", "X-Second", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		if header.Get(name) != "" {
			t.Fatalf("hop header %s survived: %#v", name, header)
		}
	}
	if header.Get("Content-Type") != "video/mp4" || header.Get("Content-Range") != "bytes 0-9/10" || header.Get("Cache-Control") == "" {
		t.Fatalf("end-to-end headers removed: %#v", header)
	}

	src := header.Clone()
	src.Add("Connection", "X-Upstream-Hop")
	src.Set("X-Upstream-Hop", "remove")
	dst := make(http.Header)
	copyResponseHeadersWithSnapshot(dst, src, "/Videos/item/stream", &Session{}, upstreamRequestSnapshot{}, "", "", "gateway")
	if dst.Get("Connection") != "" || dst.Get("X-Upstream-Hop") != "" || dst.Get("Content-Type") != "video/mp4" || dst.Get("Content-Range") != "bytes 0-9/10" {
		t.Fatalf("response headers=%#v", dst)
	}
}
