package adminapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
)

func TestMediaBufferCurrentAggregateExcludesTrafficHistory(t *testing.T) {
	r := enabledRegistry(0)
	h, cookie := buildPhase2Handler(t, r, nil)
	rr := phase2Get(t, h, cookie, "/admin/api/v1/media-buffer")
	if rr.Code != http.StatusOK {
		t.Fatalf("aggregate=%d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		BootID    string                         `json:"boot_id"`
		Now       time.Time                      `json:"now"`
		StartedAt time.Time                      `json:"started_at"`
		Aggregate telemetry.MediaBufferAggregate `json:"media_buffer"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.BootID != r.BootID() || body.Now.IsZero() || body.StartedAt.IsZero() || body.Aggregate.Health != telemetry.MediaBufferHealthIdle || !body.Aggregate.Enabled {
		t.Fatalf("aggregate=%+v", body)
	}
	if strings.Contains(rr.Body.String(), `"series"`) || strings.Contains(rr.Body.String(), `"points"`) || strings.Contains(rr.Body.String(), `"stream_id"`) || rr.Body.Len() > 4096 {
		t.Fatalf("aggregate contains unrelated history: %s", rr.Body.String())
	}
}

func TestMediaBufferHistoryFilteredCursorDetailsAndStatuses(t *testing.T) {
	r := enabledRegistry(0)
	now := time.Now().UTC().Add(-time.Second)
	offer := func(id uint64, outcome telemetry.MediaBufferOutcome) {
		t.Helper()
		if !r.MediaBufferLive().OfferCompletion(telemetry.MediaBufferCompletion{Terminal: telemetry.MediaBufferLiveSnapshot{StreamID: id, StartedAt: now.Add(-time.Minute), ItemID: "video", SourceRef: "source-proof"}, Outcome: string(outcome), CompletedAt: now}) {
			t.Fatalf("offer %d", id)
		}
	}
	for id := uint64(1); id <= 5; id++ {
		outcome := telemetry.OutcomeSuccess
		if id%2 == 1 {
			outcome = telemetry.OutcomeUpstreamError
		}
		offer(id, outcome)
	}
	h, cookie := buildPhase2Handler(t, r, nil)
	rr := phase2Get(t, h, cookie, "/admin/api/v1/media-buffer/recent?limit=2&outcome=errors")
	var page telemetry.MediaBufferRecentPage
	if rr.Code != http.StatusOK {
		t.Fatalf("first page=%d %s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].StreamID != "5" || page.Items[1].StreamID != "3" || !page.HasMore || page.NextCursor == "" || page.Capacity != 2048 || page.RetentionSeconds != 86400 || page.OldestRetainedAt == nil {
		t.Fatalf("page=%+v", page)
	}
	cursor := page.NextCursor
	offer(6, telemetry.OutcomeUpstreamError)
	rr = phase2Get(t, h, cookie, "/admin/api/v1/media-buffer/recent?limit=2&cursor="+cursor)
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || len(page.Items) != 1 || page.Items[0].StreamID != "1" || page.HasMore {
		t.Fatalf("second page=%d %+v", rr.Code, page)
	}
	rr = phase2Get(t, h, cookie, "/admin/api/v1/media-buffer/recent/5?boot_id="+r.BootID())
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"completion_id":"5"`) || !strings.Contains(rr.Body.String(), `"source_ref":"source-proof"`) {
		t.Fatalf("detail=%d %s", rr.Code, rr.Body.String())
	}
	assertError := func(path string, status int, code string) {
		t.Helper()
		rr := phase2Get(t, h, cookie, path)
		if rr.Code != status || !strings.Contains(rr.Body.String(), `"error":"`+code+`"`) {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
	}
	assertError("/admin/api/v1/media-buffer/recent?cursor=bad!", 400, "invalid_cursor")
	assertError("/admin/api/v1/media-buffer/recent?cursor="+cursor+"&outcome=success", 400, "invalid_cursor")
	assertError("/admin/api/v1/media-buffer/recent?outcome=oops", 400, "invalid_outcome")
	assertError("/admin/api/v1/media-buffer/recent?from=bad", 400, "invalid_window")
	q := url.Values{"from": {now.Add(-25 * time.Hour).Format(time.RFC3339Nano)}, "to": {now.Format(time.RFC3339Nano)}}
	assertError("/admin/api/v1/media-buffer/recent?"+q.Encode(), 400, "invalid_window")
	parsed, ok := decodeMediaBufferRecentCursor(cursor)
	if !ok {
		t.Fatal("decode own cursor")
	}
	parsed.BootID = "previous-boot"
	assertError("/admin/api/v1/media-buffer/recent?cursor="+encodeMediaBufferRecentCursor(parsed), 409, "stale_cursor")
	assertError("/admin/api/v1/media-buffer/recent/5?boot_id=previous-boot", 409, "stale_boot")
	assertError("/admin/api/v1/media-buffer/recent/0", 400, "invalid_completion_id")
	assertError("/admin/api/v1/media-buffer/recent/999999", 404, "completion_not_found")
	for id := uint64(7); id < 2060; id++ {
		offer(id, telemetry.OutcomeSuccess)
		if id%256 == 0 {
			_ = r.MediaBufferRecent(1)
		}
	}
	_ = r.MediaBufferRecent(1)
	assertError("/admin/api/v1/media-buffer/recent?cursor="+cursor, 410, "expired_cursor")
	assertError("/admin/api/v1/media-buffer/recent/5", 410, "completion_expired")
}
