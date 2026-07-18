package gateway

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOnceReadCloserReturnsFirstCloseResult(t *testing.T) {
	closeErr := errors.New("close failed")
	underlying := newLifecycleCloseBody("body", closeErr)
	response := &http.Response{Body: underlying}
	owner := wrapResponseBodyOnce(response)
	if owner == nil || response.Body != owner || wrapResponseBodyOnce(response) != owner {
		t.Fatal("response body did not retain one shared owner")
	}
	first := response.Body.Close()
	second := owner.Close()
	if !errors.Is(first, closeErr) || first != second || underlying.closeCount() != 1 {
		t.Fatalf("first=%v second=%v closes=%d", first, second, underlying.closeCount())
	}
}

func TestOnceReadCloserNilAndDoErrorResponseSafety(t *testing.T) {
	if wrapResponseBodyOnce(nil) != nil {
		t.Fatal("nil response produced an owner")
	}
	if wrapResponseBodyOnce(&http.Response{}) != nil {
		t.Fatal("nil body produced an owner")
	}
	if isConcurrentPlaybackDenial(nil) || isConcurrentPlaybackDenial(&http.Response{}) {
		t.Fatal("nil response or body matched playback denial")
	}
	closeErr := errors.New("close after Do error")
	underlying := newLifecycleCloseBody("partial", closeErr)
	response := &http.Response{StatusCode: http.StatusBadGateway, Body: underlying}
	owner := wrapResponseBodyOnce(response)
	if status := closeResponseOnError(response); status != http.StatusBadGateway {
		t.Fatalf("status=%d", status)
	}
	if err := owner.Close(); !errors.Is(err, closeErr) || underlying.closeCount() != 1 {
		t.Fatalf("close=%v count=%d", err, underlying.closeCount())
	}
}

func TestProxyCloseLifecycleInitialSuccess(t *testing.T) {
	body := newLifecycleCloseBody(`{"ok":true}`, nil)
	var response *http.Response
	transport := lifecycleRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/emby/Items" {
			t.Fatalf("unexpected request %s", req.URL.Path)
		}
		response = lifecycleResponse(http.StatusOK, "application/json", body)
		return response, nil
	})
	server := newLifecycleTestServer(t, transport)
	writer := httptest.NewRecorder()
	server.ServeHTTP(writer, lifecycleGatewayRequest("/emby/Items?api_key=gateway-token"))
	if writer.Code != http.StatusOK || !strings.Contains(writer.Body.String(), `"ok":true`) || body.closeCount() != 1 {
		t.Fatalf("status=%d body=%q closes=%d", writer.Code, writer.Body.String(), body.closeCount())
	}
	requireLifecycleOwner(t, response)
}

func TestProxyUnauthorizedRetryCloseLifecycle(t *testing.T) {
	initial := newLifecycleCloseBody("stale", nil)
	probe := newLifecycleCloseBody("stale", nil)
	auth := newLifecycleCloseBody(`{"AccessToken":"new-token","ServerId":"backend-server","User":{"Id":"backend-user","Name":"shared"}}`, nil)
	logout := newLifecycleCloseBody("", nil)
	replacement := newLifecycleCloseBody(`{"ok":true}`, nil)
	var initialResponse, probeResponse, replacementResponse *http.Response
	itemsCalls := 0
	transport := lifecycleRoundTripper(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/emby/Items":
			itemsCalls++
			if itemsCalls == 1 {
				initialResponse = lifecycleResponse(http.StatusUnauthorized, "text/plain", initial)
				return initialResponse, nil
			}
			replacementResponse = lifecycleResponse(http.StatusOK, "application/json", replacement)
			return replacementResponse, nil
		case "/emby/System/Info":
			probeResponse = lifecycleResponse(http.StatusUnauthorized, "text/plain", probe)
			return probeResponse, nil
		case "/emby/Users/AuthenticateByName":
			return lifecycleResponse(http.StatusOK, "application/json", auth), nil
		case "/emby/Sessions/Logout":
			return lifecycleResponse(http.StatusNoContent, "", logout), nil
		default:
			t.Fatalf("unexpected request %s", req.URL.Path)
			return nil, errors.New("unexpected request")
		}
	})
	server := newLifecycleTestServer(t, transport)
	writer := httptest.NewRecorder()
	server.ServeHTTP(writer, lifecycleGatewayRequest("/emby/Items?api_key=gateway-token"))
	if writer.Code != http.StatusOK || itemsCalls != 2 {
		t.Fatalf("status=%d item calls=%d body=%q", writer.Code, itemsCalls, writer.Body.String())
	}
	for name, body := range map[string]*lifecycleCloseBody{
		"discarded initial":  initial,
		"unauthorized probe": probe,
		"auth refresh":       auth,
		"logout":             logout,
		"retry replacement":  replacement,
	} {
		if body.closeCount() != 1 {
			t.Fatalf("%s close count=%d", name, body.closeCount())
		}
	}
	requireDistinctLifecycleOwners(t, initialResponse, probeResponse, replacementResponse)
}

func TestDownloadForbiddenFallbackCloseLifecycle(t *testing.T) {
	original := newLifecycleCloseBody("forbidden", nil)
	playback := newLifecycleCloseBody(`{"MediaSources":[{"Id":"source-1","Name":"movie","Container":"mkv","DirectStreamUrl":"/Videos/item-1/original.mkv?MediaSourceId=source-1","SupportsDirectStream":true}]}`, nil)
	replacement := newLifecycleCloseBody("data", nil)
	var originalResponse, playbackResponse, replacementResponse *http.Response
	transport := lifecycleRoundTripper(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/emby/Items/item-1/Download":
			originalResponse = lifecycleResponse(http.StatusForbidden, "text/plain", original)
			return originalResponse, nil
		case "/emby/Items/item-1/PlaybackInfo":
			playbackResponse = lifecycleResponse(http.StatusOK, "application/json", playback)
			return playbackResponse, nil
		case "/emby/Videos/item-1/original.mkv":
			replacementResponse = lifecycleResponse(http.StatusOK, "application/octet-stream", replacement)
			return replacementResponse, nil
		default:
			t.Fatalf("unexpected request %s", req.URL.Path)
			return nil, errors.New("unexpected request")
		}
	})
	server := newLifecycleTestServer(t, transport)
	writer := httptest.NewRecorder()
	server.ServeHTTP(writer, lifecycleGatewayRequest("/emby/Items/item-1/Download?MediaSourceId=source-1&api_key=gateway-token"))
	if writer.Code != http.StatusOK || writer.Body.String() != "data" {
		t.Fatalf("status=%d body=%q", writer.Code, writer.Body.String())
	}
	for name, body := range map[string]*lifecycleCloseBody{"discarded forbidden": original, "playback info": playback, "fallback replacement": replacement} {
		if body.closeCount() != 1 {
			t.Fatalf("%s close count=%d", name, body.closeCount())
		}
	}
	requireDistinctLifecycleOwners(t, originalResponse, playbackResponse, replacementResponse)
}

func TestOnceReadCloserPlaybackClassificationPreservesOwnerIdentity(t *testing.T) {
	oversized := strings.Repeat("x", (48<<10)+1)
	for _, tt := range []struct {
		name string
		body io.Reader
		want string
	}{
		{name: "nonmatching", body: strings.NewReader(`{"reason_code":"other"}`), want: `{"reason_code":"other"}`},
		{name: "oversized", body: strings.NewReader(oversized), want: oversized},
	} {
		t.Run(tt.name, func(t *testing.T) {
			underlying := &lifecycleCloseBody{Reader: tt.body}
			response := &http.Response{Body: underlying}
			owner := wrapResponseBodyOnce(response)
			if isConcurrentPlaybackDenial(response) || response.Body != owner {
				t.Fatal("classification changed owner or matched unexpectedly")
			}
			data, err := io.ReadAll(response.Body)
			if err != nil || string(data) != tt.want {
				t.Fatalf("replay=%q err=%v", data, err)
			}
			_ = response.Body.Close()
			if underlying.closeCount() != 1 {
				t.Fatalf("close count=%d", underlying.closeCount())
			}
		})
	}

	const original = `{"reason_code":"max_concurrent_sessions_exceeded"}`
	underlying := &lifecycleCloseBody{Reader: &recoveringErrorReader{remaining: []byte(original), first: 9}}
	response := &http.Response{Body: underlying}
	owner := wrapResponseBodyOnce(response)
	if isConcurrentPlaybackDenial(response) || response.Body != owner {
		t.Fatal("read-error classification changed owner or matched")
	}
	first := make([]byte, len(original))
	n, err := response.Body.Read(first)
	if string(first[:n]) != original[:9] || err == nil || err.Error() != "temporary read error" {
		t.Fatalf("first replay=%q err=%v", first[:n], err)
	}
	remainder, err := io.ReadAll(response.Body)
	if err != nil || string(first[:n])+string(remainder) != original {
		t.Fatalf("remainder=%q err=%v", remainder, err)
	}
	_ = response.Body.Close()
	if underlying.closeCount() != 1 {
		t.Fatalf("read-error close count=%d", underlying.closeCount())
	}

	denialBody := newLifecycleCloseBody(`{"reason_code":"max_concurrent_sessions_exceeded"}`, nil)
	denialResponse := &http.Response{Body: denialBody}
	denialOwner := wrapResponseBodyOnce(denialResponse)
	if !isConcurrentPlaybackDenial(denialResponse) || denialResponse.Body != denialOwner {
		t.Fatal("recognized denial changed owner or did not match")
	}
	_ = denialResponse.Body.Close()
	if denialBody.closeCount() != 1 {
		t.Fatalf("recognized denial close count=%d", denialBody.closeCount())
	}
}

type lifecycleRoundTripper func(*http.Request) (*http.Response, error)

func (f lifecycleRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := f(req)
	if resp != nil && resp.Request == nil {
		resp.Request = req
	}
	return resp, err
}

type lifecycleCloseBody struct {
	io.Reader
	closeErr error
	closes   atomic.Int32
}

func newLifecycleCloseBody(body string, closeErr error) *lifecycleCloseBody {
	return &lifecycleCloseBody{Reader: strings.NewReader(body), closeErr: closeErr}
}

func (b *lifecycleCloseBody) Close() error {
	b.closes.Add(1)
	return b.closeErr
}

func (b *lifecycleCloseBody) closeCount() int32 { return b.closes.Load() }

func lifecycleResponse(status int, contentType string, body io.ReadCloser) *http.Response {
	header := make(http.Header)
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	return &http.Response{StatusCode: status, Header: header, Body: body, ContentLength: -1}
}

func newLifecycleTestServer(t *testing.T, transport http.RoundTripper) *Server {
	t.Helper()
	store := testStore("http://backend.test/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	return NewServer(Config{GatewayBasePath: "/emby", HTTPClient: &http.Client{Transport: transport}}, store)
}

func lifecycleGatewayRequest(path string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "http://gateway.test"+path, nil)
}

func requireLifecycleOwner(t *testing.T, response *http.Response) *onceReadCloser {
	t.Helper()
	if response == nil {
		t.Fatal("response was not retained")
	}
	owner, ok := response.Body.(*onceReadCloser)
	if !ok || owner == nil {
		t.Fatalf("response body type=%T, want *onceReadCloser", response.Body)
	}
	return owner
}

func requireDistinctLifecycleOwners(t *testing.T, responses ...*http.Response) {
	t.Helper()
	seen := make(map[*onceReadCloser]struct{}, len(responses))
	for _, response := range responses {
		owner := requireLifecycleOwner(t, response)
		if _, exists := seen[owner]; exists {
			t.Fatal("different responses shared one close owner")
		}
		seen[owner] = struct{}{}
	}
}
