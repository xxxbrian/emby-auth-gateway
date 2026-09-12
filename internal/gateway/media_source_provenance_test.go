package gateway

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
)

type mediaSourceCheckingWriter struct {
	*httptest.ResponseRecorder
	check func()
}

func (w *mediaSourceCheckingWriter) Write(p []byte) (int, error) {
	if w.check != nil {
		w.check()
	}
	return w.ResponseRecorder.Write(p)
}

func TestMediaSourceProvenanceCapturesSelectedResponseForBothCopyModes(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		name := "synchronous"
		if buffered {
			name = "buffered"
		}
		t.Run(name, func(t *testing.T) {
			store := NewMemoryStore()
			var upstreamCalls atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				if r.URL.Path != "/emby/Videos/movie-1/stream" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				// Change the store only after the actual upstream request was selected.
				// Observation must retain that selected identity, not re-read this value.
				store.mu.Lock()
				source := store.UpstreamSources["source"]
				source.BackendUserID = "next-user"
				source.BackendToken = "next-token"
				store.UpstreamSources["source"] = source
				store.mu.Unlock()
				w.Header().Set("Content-Type", "video/mp4")
				w.Header().Set("Content-Length", "5")
				_, _ = w.Write([]byte("media"))
			}))
			defer backend.Close()
			configureTestUpstream(store, backend.URL+"/emby")
			store.Sessions[HashToken("gateway-token")] = testSession()
			meter := telemetry.NewByteMeter()
			reg := telemetry.New(nil).MediaBufferLive()
			cfg := Config{GatewayBasePath: "/emby", Meter: meter}
			if buffered {
				cfg.MediaBuffer = &MediaBuffer{controller: mustMediaBufferCopyController(t, mediaBufferChunkSize)}
				cfg.MediaBufferLive = reg
			}
			server := NewServer(cfg, store)
			want := MediaSourceRef("backend-server", "backend-user")
			checked := false
			writer := &mediaSourceCheckingWriter{ResponseRecorder: httptest.NewRecorder(), check: func() {
				checked = true
				active := meter.ActiveTransfers()
				if len(active) != 1 || active[0].ItemID != "movie-1" || active[0].SourceRef != want {
					t.Errorf("transfer lost selected source: %+v", active)
				}
				if buffered && len(active) == 1 {
					if active[0].MediaBuffer == nil {
						t.Error("buffer link missing")
						return
					}
					live, ok := reg.Detail(active[0].MediaBuffer.StreamID)
					if !ok {
						t.Error("live missing")
						return
					}
					v := live.MediaBufferLiveSnapshot()
					if v.SourceRef != want || v.ItemID != "movie-1" {
						t.Errorf("live source=%+v", v)
					}
				}
			}}
			request := httptest.NewRequest(http.MethodGet, "http://gateway/emby/Videos/movie-1/stream?api_key=gateway-token", nil)
			server.ServeHTTP(writer, request)
			if writer.Code != http.StatusOK || writer.Body.String() != "media" || !checked || upstreamCalls.Load() != 1 {
				t.Fatalf("copy=%d %q checked=%v calls=%d", writer.Code, writer.Body.String(), checked, upstreamCalls.Load())
			}
			if meter.ActiveTransferCount() != 0 {
				t.Fatal("transfer leaked")
			}
			if buffered {
				completion, ok := reg.TryCompletion()
				if !ok || completion.Terminal.SourceRef != want || completion.Terminal.ItemID != "movie-1" {
					t.Fatalf("completion lost source: %+v ok=%v", completion, ok)
				}
			}
		})
	}
}

func TestMediaSourceProvenanceMissingEvidenceAndRefresh(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://gateway/emby/Videos/item/stream", nil)
	if observedMediaSource(req) != "" || observedMediaSource(nil) != "" {
		t.Fatal("unobserved request gained source")
	}
	source := testUpstreamSnapshot("https://upstream.invalid/emby")
	captured := withObservedMediaSource(req, source)
	source.token = "refreshed-token"
	source.identity.DeviceID = "refreshed-device"
	refreshed := withObservedMediaSource(req, source)
	if observedMediaSource(captured) == "" || observedMediaSource(captured) != observedMediaSource(refreshed) {
		t.Fatal("token refresh changed catalog source")
	}
	source.userID = "different-user"
	if observedMediaSource(withObservedMediaSource(req, source)) == observedMediaSource(captured) {
		t.Fatal("backend identity change did not change source")
	}
	if observedMediaSource(withObservedMediaSource(req, upstreamRequestSnapshot{})) != "" {
		t.Fatal("missing source gained identity")
	}
}

func TestMediaSourceProvenancePlaybackCaptureAndMissingRuntime(t *testing.T) {
	for _, configured := range []bool{false, true} {
		name := "missing"
		if configured {
			name = "configured"
		}
		t.Run(name, func(t *testing.T) {
			store := NewMemoryStore()
			if configured {
				configureTestUpstream(store, "https://upstream.invalid/emby")
			}
			emitter := observe.NewEmitter(8)
			defer emitter.Close()
			server := NewServer(Config{Emitter: emitter}, store)
			session := testSession()
			body := `{"ItemId":"movie-1","Item":{"Id":"movie-1","Name":"A movie","Type":"Movie","RunTimeTicks":12000000000},"PositionTicks":300000000}`
			req := httptest.NewRequest(http.MethodPost, "http://gateway/emby/Sessions/Playing", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			if err := server.recordPlaybackRequest(req, "/Sessions/Playing", session, ""); err != nil {
				t.Fatal(err)
			}
			select {
			case ev := <-emitter.Events():
				want := ""
				if configured {
					want = MediaSourceRef("backend-server", "backend-user")
				}
				if ev.Kind != observe.KindPlayback || ev.SourceRef != want || ev.ItemID != "movie-1" {
					t.Fatalf("playback event=%+v", ev)
				}
				if strings.Contains(ev.SourceRef, "backend-token") {
					t.Fatal("source exposed token")
				}
			default:
				t.Fatal("missing playback event")
			}
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			if server.localMediaSource(canceled) != "" {
				t.Fatal("failed runtime load guessed source")
			}
		})
	}
}
