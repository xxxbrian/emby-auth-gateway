package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminauth"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminquery"
	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
)

type adminLiveState struct {
	id       uint64
	terminal bool
	snapshot telemetry.MediaBufferLiveSnapshot
}

func (s *adminLiveState) MediaBufferRawStreamID() uint64 { return s.id }
func (s *adminLiveState) MediaBufferTerminal() bool      { return s.terminal }
func (s *adminLiveState) MediaBufferLiveSnapshot() telemetry.MediaBufferLiveSnapshot {
	v := s.snapshot
	v.StreamID = s.id
	v.Terminal = s.terminal
	return v
}
func (s *adminLiveState) MediaBufferLiveBytes() (int64, int64) { return 123, 100 }

func buildPhase2Handler(t *testing.T, reg *telemetry.Registry, available func() bool) (http.Handler, *http.Cookie) {
	t.Helper()
	app := newTestApp(t)
	superuser := createSuperuser(t, app, "phase2@example.test", "SuperSecret1!")
	token, err := superuser.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	sessions := adminauth.NewStore(20)
	server, err := New(Config{App: app, Sessions: sessions, Query: adminquery.New(app, 2), Telemetry: reg, MediaBufferEnabled: available, BootID: "disabled-boot"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	server.Mount(r)
	h, err := r.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"token": token})
	rr := httptest.NewRecorder()
	req := withSameOrigin(httptest.NewRequest(http.MethodPost, "/admin/api/v1/session", bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("create session: %d %s", rr.Code, rr.Body.String())
	}
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name == adminauth.CookieDev || cookie.Name == adminauth.CookieSecure {
			return h, cookie
		}
	}
	t.Fatal("missing session cookie")
	return nil, nil
}

func phase2Get(t *testing.T, h http.Handler, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	return rr
}

func enabledRegistry(active int) *telemetry.Registry {
	r := telemetry.New(nil)
	r.SetMediaBufferProvider(func() telemetry.MediaBufferControllerSnapshot {
		return telemetry.MediaBufferControllerSnapshot{Available: true, Enabled: true, HardBudgetBytes: 65536, AllocatedBytes: 32768, OwnedBytes: 32768, ActiveRequests: active}
	})
	return r
}

func TestMediaBufferDisabledAndProviderUnavailable(t *testing.T) {
	disabled := telemetry.New(nil)
	disabled.SetMediaBufferProvider(func() telemetry.MediaBufferControllerSnapshot {
		return telemetry.MediaBufferControllerSnapshot{Available: true, Enabled: false}
	})
	h, cookie := buildPhase2Handler(t, disabled, nil)
	for _, path := range []string{"/admin/api/v1/media-buffer/streams", "/admin/api/v1/media-buffer/streams/1?boot_id=" + disabled.BootID(), "/admin/api/v1/media-buffer/series?window=1h", "/admin/api/v1/media-buffer/recent"} {
		rr := phase2Get(t, h, cookie, path)
		if rr.Code != http.StatusOK {
			t.Fatalf("disabled %s: %d %s", path, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), disabled.BootID()) || (!strings.Contains(rr.Body.String(), "[]") && !strings.Contains(rr.Body.String(), `"item":null`)) {
			t.Fatalf("disabled payload %s", rr.Body.String())
		}
	}

	unavailable := telemetry.New(nil)
	h, cookie = buildPhase2Handler(t, unavailable, func() bool { return true })
	for _, path := range []string{"/admin/api/v1/media-buffer/streams", "/admin/api/v1/media-buffer/streams/1", "/admin/api/v1/media-buffer/series", "/admin/api/v1/media-buffer/recent"} {
		rr := phase2Get(t, h, cookie, path)
		if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), `"error":"provider_unavailable"`) {
			t.Fatalf("unavailable %s: %d %s", path, rr.Code, rr.Body.String())
		}
	}
}

func TestMediaBufferCursorRawTombstoneAndStatusCodes(t *testing.T) {
	r := enabledRegistry(2)
	now := time.Now().UTC().Add(-time.Second)
	if !r.MediaBufferLive().Register(&adminLiveState{id: 1, terminal: true, snapshot: telemetry.MediaBufferLiveSnapshot{StartedAt: now}}) || !r.MediaBufferLive().Register(&adminLiveState{id: 2, snapshot: telemetry.MediaBufferLiveSnapshot{StartedAt: now}}) || !r.MediaBufferLive().Register(&adminLiveState{id: 3, snapshot: telemetry.MediaBufferLiveSnapshot{StartedAt: now}}) {
		t.Fatal("register fixtures")
	}
	h, cookie := buildPhase2Handler(t, r, nil)

	assertError := func(path string, status int, code string) {
		t.Helper()
		rr := phase2Get(t, h, cookie, path)
		if rr.Code != status || !strings.Contains(rr.Body.String(), `"error":"`+code+`"`) {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
	}
	assertError("/admin/api/v1/media-buffer/streams?cursor=bad!", http.StatusBadRequest, "invalid_cursor")
	assertError("/admin/api/v1/media-buffer/streams?cursor="+encodeMediaBufferCursor("old", 1), http.StatusConflict, "stale_cursor")
	assertError("/admin/api/v1/media-buffer/streams/xyz?boot_id="+r.BootID(), http.StatusBadRequest, "invalid_stream_id")
	assertError("/admin/api/v1/media-buffer/streams/0?boot_id="+r.BootID(), http.StatusBadRequest, "invalid_stream_id")
	assertError("/admin/api/v1/media-buffer/streams/2?boot_id=old", http.StatusConflict, "stale_boot")
	assertError("/admin/api/v1/media-buffer/streams/99?boot_id="+r.BootID(), http.StatusNotFound, "stream_not_found")

	rr := phase2Get(t, h, cookie, "/admin/api/v1/media-buffer/streams?limit=1")
	if rr.Code != http.StatusOK {
		t.Fatalf("page: %d %s", rr.Code, rr.Body.String())
	}
	var page telemetry.MediaBufferLivePageDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("tombstone page=%+v", page)
	}
	cursor, ok := decodeMediaBufferCursor(page.NextCursor)
	if !ok || cursor.RawID != 1 || cursor.BootID != r.BootID() {
		t.Fatalf("cursor=%+v ok=%v", cursor, ok)
	}
	rr = phase2Get(t, h, cookie, "/admin/api/v1/media-buffer/streams?limit=1&cursor="+page.NextCursor)
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].StreamID != "2" {
		t.Fatalf("second page=%+v", page)
	}

	rr = phase2Get(t, h, cookie, "/admin/api/v1/media-buffer/streams/2?boot_id="+r.BootID())
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"boot_id":"`+r.BootID()+`"`) || !strings.Contains(rr.Body.String(), `"item":{"`) || !strings.Contains(rr.Body.String(), `"stream_id":"2"`) {
		t.Fatalf("detail: %d %s", rr.Code, rr.Body.String())
	}
}

func TestMediaBufferSeriesNormalizationGapsAndRecentOrder(t *testing.T) {
	r := enabledRegistry(0)
	now := time.Now().UTC()
	if !r.SampleMediaBufferOnce(now.Add(-2*time.Second)) || !r.SampleMediaBufferOnce(now) {
		t.Fatal("sample")
	}
	for id := uint64(1); id <= 205; id++ {
		if !r.MediaBufferLive().OfferCompletion(telemetry.MediaBufferCompletion{Terminal: telemetry.MediaBufferLiveSnapshot{StreamID: id, StartedAt: now.Add(-time.Second)}, Outcome: string(telemetry.OutcomeSuccess), CompletedAt: now}) {
			t.Fatal("completion offer")
		}
	}
	h, cookie := buildPhase2Handler(t, r, nil)
	for _, tc := range []struct {
		query, window, interval string
		points                  int
	}{{"", "15m", "1s", 900}, {"weird", "15m", "1s", 900}, {"1h", "1h", "1m", 60}, {"6h", "6h", "1m", 360}, {"24h", "24h", "1m", 1440}} {
		rr := phase2Get(t, h, cookie, "/admin/api/v1/media-buffer/series?window="+tc.query)
		var series telemetry.MediaBufferSeries
		if rr.Code != http.StatusOK {
			t.Fatalf("series %s: %d %s", tc.query, rr.Code, rr.Body.String())
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &series); err != nil {
			t.Fatal(err)
		}
		if series.Window != tc.window || series.Interval != tc.interval || len(series.Points) != tc.points {
			t.Fatalf("series %s=%s/%s/%d", tc.query, series.Window, series.Interval, len(series.Points))
		}
		if tc.window == "15m" {
			foundGap := false
			for _, p := range series.Points {
				if !p.Present && p.Aggregate == nil && p.Domains == nil {
					foundGap = true
					break
				}
				if p.Present && (p.Aggregate == nil || p.Domains == nil || p.Domains.Pool != "coherent" || p.Domains.Sidecar != "eventual") {
					t.Fatalf("present point has invalid domains: %+v", p)
				}
			}
			if !foundGap {
				t.Fatal("missing explicit series gap")
			}
		}
	}
	rr := phase2Get(t, h, cookie, "/admin/api/v1/media-buffer/recent?limit=2")
	var recent telemetry.MediaBufferRecentPage
	if err := json.Unmarshal(rr.Body.Bytes(), &recent); err != nil {
		t.Fatal(err)
	}
	if len(recent.Items) != 2 || recent.Items[0].StreamID != "205" || recent.Items[1].StreamID != "204" {
		t.Fatalf("recent=%+v", recent.Items)
	}
	for _, limit := range []string{"0", "bad"} {
		rr = phase2Get(t, h, cookie, "/admin/api/v1/media-buffer/recent?limit="+limit)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), `"error":"invalid_limit"`) {
			t.Fatalf("limit %s: %d %s", limit, rr.Code, rr.Body.String())
		}
	}
	rr = phase2Get(t, h, cookie, "/admin/api/v1/media-buffer/recent?limit=201")
	if rr.Code != http.StatusOK || rr.Body.Len() >= mediaBufferMaxJSON {
		t.Fatalf("clamped recent limit: %d %s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &recent); err != nil {
		t.Fatal(err)
	}
	if len(recent.Items) != mediaBufferMaxLimit {
		t.Fatalf("clamped recent items=%d", len(recent.Items))
	}
}

func TestMediaBufferPayloadPrivacyBoundsOverviewParityAndActivityLinkage(t *testing.T) {
	r := enabledRegistry(1)
	long := strings.Repeat("x", 300)
	state := &adminLiveState{id: 7, snapshot: telemetry.MediaBufferLiveSnapshot{StartedAt: time.Now().Add(-3 * time.Second), UserID: "user\x00", Username: long, Device: "device", ItemID: "item", MediaMode: "invalid", Producer: telemetry.MediaBufferTimedValue{Value: 255}, Consumer: telemetry.MediaBufferTimedValue{Value: 255}, Lifecycle: telemetry.MediaBufferTimedValue{Value: 255}, Blocker: telemetry.MediaBufferTimedValue{Value: 255}, TargetBytes: -1}}
	if !r.MediaBufferLive().Register(state) {
		t.Fatal("register")
	}
	for id := uint64(8); id <= 206; id++ {
		if !r.MediaBufferLive().Register(&adminLiveState{id: id, snapshot: telemetry.MediaBufferLiveSnapshot{StartedAt: time.Now().Add(-time.Second), UserID: long, Username: long, Device: long, ItemID: long, MediaMode: "direct"}}) {
			t.Fatalf("register %d", id)
		}
	}
	valid := r.Meter().BeginTransfer(telemetry.TransferMeta{SessionID: "valid", MediaBuffer: &telemetry.MediaBufferReference{BootID: r.BootID(), StreamID: 7}})
	stale := r.Meter().BeginTransfer(telemetry.TransferMeta{SessionID: "stale", MediaBuffer: &telemetry.MediaBufferReference{BootID: r.BootID(), StreamID: 999}})
	t.Cleanup(func() { valid.End(nil); stale.End(nil) })
	h, cookie := buildPhase2Handler(t, r, nil)

	rr := phase2Get(t, h, cookie, "/admin/api/v1/media-buffer/streams?limit=200")
	if rr.Code != http.StatusOK || rr.Body.Len() >= mediaBufferMaxJSON {
		t.Fatalf("bounded list: %d bytes=%d", rr.Code, rr.Body.Len())
	}
	text := rr.Body.String()
	for _, forbidden := range []string{"backend_url", "token", "media_url", "path", "session_hash", "completion"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Fatalf("forbidden %q in %s", forbidden, text)
		}
	}
	var page telemetry.MediaBufferLivePageDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != mediaBufferMaxLimit || page.Items[0].Username == nil || len(*page.Items[0].Username) > 256 || page.Items[0].MediaMode != telemetry.MediaBufferModeUnknown || page.Items[0].TargetBytes != 0 || !page.Items[0].ProducerState.Valid() || !page.Items[0].ConsumerState.Valid() || !page.Items[0].State.Valid() || !page.Items[0].AllocationBlocker.Valid() {
		t.Fatalf("defensive item=%+v", page.Items)
	}
	defaultPage := phase2Get(t, h, cookie, "/admin/api/v1/media-buffer/streams")
	if defaultPage.Code != http.StatusOK || defaultPage.Body.Len() >= mediaBufferMaxJSON {
		t.Fatalf("default list: %d bytes=%d", defaultPage.Code, defaultPage.Body.Len())
	}
	if err := json.Unmarshal(defaultPage.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != mediaBufferDefaultLimit {
		t.Fatalf("default list items=%d", len(page.Items))
	}

	overview := phase2Get(t, h, cookie, "/admin/api/v1/overview")
	if overview.Code != http.StatusOK || !strings.Contains(overview.Body.String(), `"unallocated_optional_bytes"`) || strings.Contains(overview.Body.String(), `"stream_id"`) {
		t.Fatalf("overview=%d %s", overview.Code, overview.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/admin/api/v1/metrics/stream", nil).WithContext(ctx)
	req.AddCookie(cookie)
	w := newFirstSSEWriter()
	done := make(chan struct{})
	go func() { h.ServeHTTP(w, req); close(done) }()
	frame := <-w.firstFrame
	cancel()
	<-done
	var streamed, direct map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(frame, []byte("data: "))), &streamed); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(overview.Body.Bytes(), &direct); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(streamed["media_buffer"], direct["media_buffer"]) {
		t.Fatalf("SSE/overview media_buffer differ")
	}

	activity := phase2Get(t, h, cookie, "/admin/api/v1/activity/transfers")
	var payload struct {
		Items []adminTransfer `json:"items"`
	}
	if err := json.Unmarshal(activity.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	linked, nulls := 0, 0
	for _, item := range payload.Items {
		if item.MediaBuffer == nil {
			nulls++
		} else {
			linked++
			if item.MediaBuffer.StreamID != 7 || item.MediaBuffer.BootID != r.BootID() {
				t.Fatalf("link=%+v", item.MediaBuffer)
			}
		}
	}
	if linked != 1 || nulls != 1 {
		t.Fatalf("activity linkage linked=%d null=%d body=%s", linked, nulls, activity.Body.String())
	}
	if strings.Contains(activity.Body.String(), "target_bytes") || strings.Contains(activity.Body.String(), "health") {
		t.Fatalf("activity copied buffer row: %s", activity.Body.String())
	}
}

func jsonEqual(a, b any) bool {
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(aa, bb)
}
