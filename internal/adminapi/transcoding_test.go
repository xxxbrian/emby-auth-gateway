package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
	"github.com/xxxbrian/emby-auth-gateway/internal/transcode"
)

func TestTranscodingAdminPaginationAndBootIdentity(t *testing.T) {
	r := telemetry.New(nil)
	at := time.Now().UTC()
	observation := transcode.Observation{BootID: r.BootID(), At: at, Aggregate: transcode.Aggregate{Enabled: true, WorkerLimit: 4}, Jobs: []transcode.JobView{{ID: "newer", CreatedAt: at.Add(time.Second)}, {ID: "older", CreatedAt: at}}, Recent: []transcode.JobView{{ID: "finished", State: "stopped"}}}
	calls := 0
	r.SetTranscodingProvider(func() transcode.Observation { calls++; return observation })
	h, cookie := buildPhase2Handler(t, r, nil)
	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/admin/api/v1/transcoding/jobs", nil))
	if unauth.Code != 401 || calls != 0 {
		t.Fatal("unauthorized observation access")
	}
	rr := phase2Get(t, h, cookie, "/admin/api/v1/transcoding/jobs?limit=1")
	var first transcodingPage
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &first) != nil || len(first.Items) != 1 || first.Items[0].ID != "newer" || first.NextCursor == "" {
		t.Fatalf("first page %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("private observation was cacheable")
	}
	observation.Jobs = append([]transcode.JobView{{ID: "newest", CreatedAt: at.Add(2 * time.Second)}}, observation.Jobs...)
	rr = phase2Get(t, h, cookie, "/admin/api/v1/transcoding/jobs?limit=1&cursor="+url.QueryEscape(first.NextCursor))
	var second transcodingPage
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &second) != nil || len(second.Items) != 1 || second.Items[0].ID != "older" {
		t.Fatalf("unstable pagination %s", rr.Body.String())
	}
	rr = phase2Get(t, h, cookie, "/admin/api/v1/transcoding/jobs/finished?boot_id="+r.BootID())
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "stopped") {
		t.Fatal("recent task could not be inspected")
	}
	observation.BootID = "another-boot"
	rr = phase2Get(t, h, cookie, "/admin/api/v1/transcoding/jobs?cursor="+url.QueryEscape(first.NextCursor))
	if rr.Code != 409 {
		t.Fatal("stale pagination was accepted")
	}
	rr = phase2Get(t, h, cookie, "/admin/api/v1/transcoding/jobs/newer?boot_id="+r.BootID())
	if rr.Code != 409 {
		t.Fatal("stale detail identity was accepted")
	}
}

func TestTranscodingDisabledAdminResponse(t *testing.T) {
	h, cookie := buildPhase2Handler(t, telemetry.New(nil), nil)
	rr := phase2Get(t, h, cookie, "/admin/api/v1/transcoding/jobs")
	var page transcodingPage
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &page) != nil || page.Aggregate.Enabled || page.Items == nil || page.BootID == "" {
		t.Fatalf("disabled response %s", rr.Body.String())
	}
	rr = phase2Get(t, h, cookie, "/admin/api/v1/transcoding/jobs?limit=0")
	if rr.Code != 400 {
		t.Fatal("invalid limit accepted")
	}
}
