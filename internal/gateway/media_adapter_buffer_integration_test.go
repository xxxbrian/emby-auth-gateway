package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
)

func TestMediaAdapterResponseParticipatesInAdaptiveBufferingAndTelemetry(t *testing.T) {
	payload := bytes.Repeat([]byte("m"), 3*mediaCopyBufferSize+7)
	source := &mediaAdapterBufferBody{Reader: bytes.NewReader(payload)}
	transport := &mediaAdapterBufferTransport{source: source, contentLength: int64(len(payload))}
	store := testStore("http://backend.test/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	controller := mustMediaBufferCopyController(t, 2*mediaBufferChunkSize)
	meter := telemetry.NewByteMeter()
	server := NewServer(Config{
		GatewayBasePath: "/emby",
		HTTPClient:      &http.Client{Transport: transport},
		MediaBuffer:     &MediaBuffer{controller: controller},
		Meter:           meter,
	}, store)
	defer server.Close()

	runtime, err := store.LoadDefaultUpstreamRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	server.managedAuthUpstream = &mediaAdapterBufferAuth{runtime: runtime}
	adapter := &mediaAdapterBufferObserver{inner: server.mediaUpstream}
	server.mediaUpstream = adapter

	headerCommitted := false
	server.mediaBufferHooks = &mediaBufferServerHooks{afterHeaderCommit: func() {
		headerCommitted = true
		if adapter.calls != 1 || !adapter.wrapped {
			t.Fatalf("media adapter calls=%d wrapped=%v", adapter.calls, adapter.wrapped)
		}
		if server.ActiveMediaCopies() != 1 {
			t.Fatalf("active media copies=%d, want 1", server.ActiveMediaCopies())
		}
		snapshot := controller.Snapshot()
		if snapshot.ActiveRequests != 1 {
			t.Fatalf("active buffer requests=%d, want 1; snapshot=%+v", snapshot.ActiveRequests, snapshot)
		}
		if meter.ActiveTransferCount() != 1 {
			t.Fatalf("active transfers=%d, want 1", meter.ActiveTransferCount())
		}
	}}

	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream?api_key=gateway-token", nil)
	writer := httptest.NewRecorder()
	server.ServeHTTP(writer, request)

	if writer.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", writer.Code, writer.Body.String())
	}
	if !bytes.Equal(writer.Body.Bytes(), payload) {
		t.Fatalf("response bytes=%d, want %d", writer.Body.Len(), len(payload))
	}
	if !headerCommitted {
		t.Fatal("adaptive media response did not commit through the buffered path")
	}
	if adapter.response == nil || transport.response != adapter.response {
		t.Fatal("media adapter response identity was not preserved through ServeHTTP")
	}
	if _, ok := adapter.response.Body.(*onceReadCloser); !ok {
		t.Fatalf("media adapter response body type=%T, want *onceReadCloser", adapter.response.Body)
	}
	if source.closes.Load() != 1 {
		t.Fatalf("underlying source closes=%d, want 1", source.closes.Load())
	}
	ingress, egress := meter.Totals()
	if ingress != uint64(len(payload)) || egress != uint64(len(payload)) {
		t.Fatalf("traffic ingress/egress=%d/%d, want %d/%d", ingress, egress, len(payload), len(payload))
	}
	if meter.CompletedEgress() != uint64(len(payload)) || meter.ActiveTransferCount() != 0 {
		t.Fatalf("completed egress=%d active transfers=%d", meter.CompletedEgress(), meter.ActiveTransferCount())
	}
	if server.ActiveMediaCopies() != 0 {
		t.Fatalf("active media copies=%d, want 0", server.ActiveMediaCopies())
	}
	assertMediaBufferServerIdle(t, controller)
	if snapshot := controller.Snapshot(); snapshot.Allocated == 0 {
		t.Fatalf("adaptive buffer never allocated an optional chunk: %+v", snapshot)
	}
}

type mediaAdapterBufferObserver struct {
	inner    MediaUpstream
	response *http.Response
	calls    int
	wrapped  bool
}

func (o *mediaAdapterBufferObserver) RoundTripMedia(in mediaUpstreamRequest) (*http.Response, error) {
	o.calls++
	response, err := o.inner.RoundTripMedia(in)
	o.response = response
	if response != nil {
		_, o.wrapped = response.Body.(*onceReadCloser)
	}
	return response, err
}

func (o *mediaAdapterBufferObserver) RoundTripNegotiation(in negotiationUpstreamRequest) (*http.Response, error) {
	return o.inner.RoundTripNegotiation(in)
}

type mediaAdapterBufferTransport struct {
	source        io.ReadCloser
	contentLength int64
	response      *http.Response
}

func (t *mediaAdapterBufferTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.response = &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"video/mp4"}},
		Body:          t.source,
		ContentLength: t.contentLength,
		Request:       request,
	}
	return t.response, nil
}

type mediaAdapterBufferBody struct {
	io.Reader
	closes atomic.Int32
}

func (b *mediaAdapterBufferBody) Close() error {
	b.closes.Add(1)
	return nil
}

type mediaAdapterBufferAuth struct {
	runtime *UpstreamRuntime
}

func (a *mediaAdapterBufferAuth) Ensure(context.Context) (*UpstreamRuntime, error) {
	return a.runtime, nil
}

func (a *mediaAdapterBufferAuth) Refresh(context.Context, string) (*UpstreamRuntime, error) {
	return a.runtime, nil
}

func (*mediaAdapterBufferAuth) Probe(managedAuthProbeRequest) (UpstreamServerInfoUpdate, error) {
	return UpstreamServerInfoUpdate{}, nil
}

func (*mediaAdapterBufferAuth) Login(managedAuthLoginRequest) (UpstreamAuthUpdate, error) {
	return UpstreamAuthUpdate{}, nil
}

func (*mediaAdapterBufferAuth) Logout(managedAuthLogoutRequest) error {
	return nil
}
