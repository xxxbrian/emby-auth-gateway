package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type downloadFallbackPortSpy struct {
	t              *testing.T
	leases         MediaLeaseRegistry
	calls          []string
	mediaBodyClose atomic.Int32
	mediaErr       error
}

func (s *downloadFallbackPortSpy) RoundTripNegotiation(in negotiationUpstreamRequest) (*http.Response, error) {
	s.calls = append(s.calls, "negotiation")
	if in.Request.URL.IsAbs() || in.Request.URL.Path != "/Items/item-1/PlaybackInfo" {
		s.t.Fatalf("negotiation URL = %q", in.Request.URL.String())
	}
	var request embyPlaybackInfoRequestDTO
	if err := json.NewDecoder(in.Request.Body).Decode(&request); err != nil {
		s.t.Fatal(err)
	}
	if request.UserID != "synthetic-user" {
		s.t.Fatalf("negotiation UserId = %q", request.UserID)
	}
	if err := s.leases.RegisterAll(in.Session.GatewayTokenHash, []PlaySessionID{"play-1"}, nil); err != nil {
		return nil, err
	}
	refreshed := in.Snapshot
	refreshed.token = "refreshed-token"
	*in.SnapshotRef = refreshed
	body, _ := json.Marshal(embyPlaybackInfoResponseDTO{
		PlaySessionID: "play-1",
		MediaSources: []embyMediaSourceInfoDTO{{
			ID: "source-1", Name: "movie", Container: "mkv",
			DirectStreamURL:      "/Videos/item-1/original.mkv?MediaSourceId=source-1&PlaySessionId=play-1&api_key=backend-token",
			SupportsDirectStream: true,
			RequiredHTTPHeaders:  map[string]string{"X-Required": "yes", "Authorization": "forbidden"},
		}},
	})
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func (s *downloadFallbackPortSpy) RoundTripMedia(in mediaUpstreamRequest) (*http.Response, error) {
	s.calls = append(s.calls, "media")
	if in.Request.URL.IsAbs() || in.Request.URL.Path != "/Videos/item-1/original.mkv" {
		s.t.Fatalf("media URL = %q", in.Request.URL.String())
	}
	if in.Snapshot.token != "refreshed-token" {
		s.t.Fatalf("media snapshot token = %q", in.Snapshot.token)
	}
	if in.Request.Header.Get("Range") != "bytes=0-" || in.Request.Header.Get("X-Required") != "yes" || in.Request.Header.Get("Authorization") != "" || in.Request.Header.Get("X-Unrelated") != "" {
		s.t.Fatalf("media headers = %#v", in.Request.Header)
	}
	if s.mediaErr != nil {
		return nil, s.mediaErr
	}
	body := &countingReadCloser{Reader: bytes.NewReader([]byte("data")), closes: &s.mediaBodyClose}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
}

func TestDownloadFallbackReleasesNegotiationLeaseWhenMediaFails(t *testing.T) {
	leases := NewMediaLeaseRegistry(func() time.Time { return time.Unix(100, 0) })
	spy := &downloadFallbackPortSpy{t: t, leases: leases, mediaErr: errors.New("media failed")}
	server := &Server{cfg: Config{GatewayBasePath: "/emby", GatewayServerID: "gateway"}, mediaUpstream: spy, mediaLeases: leases}
	session := &Session{GatewayTokenHash: "owner", SyntheticUserID: "synthetic-user"}
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item-1/Download?MediaSourceId=source-1&api_key=gateway-token", nil)
	request.Header.Set("Range", "bytes=0-")

	response, err := server.tryDownloadDirectStreamFallback(request, "/Items/item-1/Download", session, upstreamRequestSnapshot{baseURL: "http://backend.test/emby", userID: "backend-user", token: "backend-token", identity: BackendClientIdentity{DeviceID: "device"}}, "gateway-token")
	if !errors.Is(err, errDownloadFallbackUnavailable) || response != nil {
		t.Fatalf("response/error = %#v / %v", response, err)
	}
	if _, err := leases.Validate("owner", "play-1", "", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lease after failure = %v", err)
	}
}

type countingReadCloser struct {
	io.Reader
	closes *atomic.Int32
}

func (c *countingReadCloser) Close() error {
	c.closes.Add(1)
	return nil
}

func TestDownloadFallbackUsesPurposePortsAndReleasesLeaseOnCloseOnce(t *testing.T) {
	leases := NewMediaLeaseRegistry(func() time.Time { return time.Unix(100, 0) })
	spy := &downloadFallbackPortSpy{t: t, leases: leases, calls: []string{"media(download)"}}
	server := &Server{cfg: Config{GatewayBasePath: "/emby", GatewayServerID: "gateway"}, mediaUpstream: spy, mediaLeases: leases}
	session := &Session{GatewayTokenHash: "owner", SyntheticUserID: "synthetic-user"}
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item-1/Download?MediaSourceId=source-1&api_key=gateway-token", nil)
	request.Header.Set("Range", "bytes=0-")
	request.Header.Set("X-Unrelated", "drop")

	response, err := server.tryDownloadDirectStreamFallback(request, "/Items/item-1/Download", session, upstreamRequestSnapshot{baseURL: "http://backend.test/emby", userID: "backend-user", token: "backend-token", identity: BackendClientIdentity{DeviceID: "device"}}, "gateway-token")
	if err != nil {
		t.Fatal(err)
	}
	if len(spy.calls) != 3 || spy.calls[0] != "media(download)" || spy.calls[1] != "negotiation" || spy.calls[2] != "media" {
		t.Fatalf("port calls = %#v", spy.calls)
	}
	if _, err := leases.Validate("owner", "play-1", "", time.Time{}); err != nil {
		t.Fatalf("lease before close: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if spy.mediaBodyClose.Load() != 1 {
		t.Fatalf("media body closes = %d", spy.mediaBodyClose.Load())
	}
	if _, err := leases.Validate("owner", "play-1", "", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lease after close = %v", err)
	}
}
