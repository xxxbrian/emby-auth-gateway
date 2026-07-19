package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type publicMetadataSpy struct {
	calls    int
	body     *publicMetadataBody
	response *http.Response
}

func (s *publicMetadataSpy) RoundTripMetadata(in metadataUpstreamRequest) (*http.Response, error) {
	s.calls++
	if !in.Internal || !in.Public || in.Session != nil || in.Request.URL.Path != "/System/Info/Public" || in.Request.URL.RawQuery != "" || in.Snapshot.token != "" || in.Snapshot.userID != "" {
		return nil, ErrForbidden
	}
	s.body = &publicMetadataBody{Reader: strings.NewReader(`{"Id":"backend-server","ServerName":"Backend","Version":"4.9"}`)}
	s.response = &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: s.body, Request: in.Request}
	return s.response, nil
}

type publicMetadataBody struct {
	io.Reader
	closeCount int
}

func (b *publicMetadataBody) Close() error {
	b.closeCount++
	return nil
}

func TestPublicSystemInfoProbeUsesMetadataPortWithoutDirectClient(t *testing.T) {
	store := testStore("http://backend.invalid/emby")
	server := NewServer(Config{HTTPClient: &http.Client{Transport: phase5PanicTransport{}}}, store)
	defer server.Close()
	spy := &publicMetadataSpy{}
	server.metadataUpstream = spy
	runtime, err := store.LoadDefaultUpstreamRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := server.probeUpstreamPublic(context.Background(), runtime)
	if err != nil || spy.calls != 1 || metadata.ServerID != "backend-server" || metadata.Name != "Backend" || metadata.Version != "4.9" {
		t.Fatalf("metadata=%#v err=%v calls=%d", metadata, err, spy.calls)
	}
	owner, ok := spy.response.Body.(*onceReadCloser)
	if !ok {
		t.Fatalf("response body type=%T, want *onceReadCloser", spy.response.Body)
	}
	if spy.body.closeCount != 1 {
		t.Fatalf("source close count=%d, want 1", spy.body.closeCount)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if spy.body.closeCount != 1 {
		t.Fatalf("source close count after repeated close=%d, want 1", spy.body.closeCount)
	}
}
