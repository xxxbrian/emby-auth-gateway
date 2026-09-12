package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/subtitles"
	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
)

func TestSubtitleObservationRequiresAdminAndContainsOnlySnapshotData(t *testing.T) {
	r := telemetry.New(nil)
	at := time.Now().UTC()
	calls := 0
	r.SetSubtitlesProvider(func() subtitles.Snapshot {
		calls++
		return subtitles.Snapshot{
			Enabled: true, BootID: r.BootID(), Workers: 1, Active: 1,
			CacheBudget: 128 << 20, SourceCacheBudget: 256 << 20,
			SourceReadBytes: 8192, SourceHitBytes: 4096,
			Jobs: []subtitles.JobView{{
				ID: "job-one", ItemID: "item-one", SourceID: "source-one", SourceName: "https://upstream.invalid/video?api_key=private", SourceSize: 50 << 30,
				State: "preparing", CreatedAt: at, UpdatedAt: at, Viewers: 2,
				Tracks: []subtitles.TrackView{
					{Index: 4, Language: "zh", Name: "Chinese", State: "ready", Format: "vtt", Mode: "indexed", Persistent: true, Bytes: 1024, Cues: 42, ReadBytes: 8192, Requests: 3},
					{Index: 5, Language: "en", Name: "English", State: "unavailable", Reason: "upstream_empty"},
				},
			}},
		}
	})
	h, cookie := buildPhase2Handler(t, r, nil)
	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/admin/api/v1/subtitles", nil))
	if unauth.Code != http.StatusUnauthorized || calls != 0 {
		t.Fatal("unauthenticated request reached subtitle observation")
	}
	rr := phase2Get(t, h, cookie, "/admin/api/v1/subtitles")
	var page subtitlePage
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &page) != nil || !page.Enabled || len(page.Jobs) != 1 || calls != 1 {
		t.Fatalf("subtitle response: %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Cache-Control"), "no-store") || strings.Contains(rr.Body.String(), "api_key") || strings.Contains(rr.Body.String(), "upstream.invalid") {
		t.Fatal("private snapshot was cacheable or contained a source URL")
	}
	if page.Jobs[0].Tracks[0].Cues != 42 || page.Jobs[0].Tracks[0].ReadBytes != 8192 || page.Jobs[0].Tracks[1].Reason != "upstream_empty" || page.SourceHitBytes != 4096 {
		t.Fatal("observation lost preparation outcome or byte accounting")
	}
	if page.Jobs[0].SourceSize != 50<<30 || page.Jobs[0].Tracks[0].Mode != "indexed" || !page.Jobs[0].Tracks[0].Persistent {
		t.Fatal("observation lost source size or extraction persistence")
	}
}

func TestSubtitleObservationDisabledWithoutProvider(t *testing.T) {
	for _, registry := range []*telemetry.Registry{nil, telemetry.New(nil)} {
		h, cookie := buildPhase2Handler(t, registry, nil)
		rr := phase2Get(t, h, cookie, "/admin/api/v1/subtitles")
		var page subtitlePage
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &page) != nil || page.Enabled || page.Jobs == nil || page.TotalJobs != 0 || page.BootID == "" {
			t.Fatalf("disabled response: %d %s", rr.Code, rr.Body.String())
		}
	}
}

func TestSubtitleObservationPaginationAndRestart(t *testing.T) {
	r := telemetry.New(nil)
	at := time.Now().UTC()
	snapshot := subtitles.Snapshot{Enabled: true, BootID: r.BootID(), Jobs: []subtitles.JobView{
		{ID: "older", CreatedAt: at.Add(-time.Minute)},
		{ID: "same-a", CreatedAt: at},
		{ID: "same-b", CreatedAt: at},
	}}
	r.SetSubtitlesProvider(func() subtitles.Snapshot { return snapshot })
	h, cookie := buildPhase2Handler(t, r, nil)
	rr := phase2Get(t, h, cookie, "/admin/api/v1/subtitles?limit=1")
	var first subtitlePage
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &first) != nil || len(first.Jobs) != 1 || first.Jobs[0].ID != "same-b" || first.NextCursor == "" {
		t.Fatalf("first page: %d %s", rr.Code, rr.Body.String())
	}
	snapshot.Jobs = append(snapshot.Jobs, subtitles.JobView{ID: "newer", CreatedAt: at.Add(time.Minute)})
	rr = phase2Get(t, h, cookie, "/admin/api/v1/subtitles?limit=1&cursor="+url.QueryEscape(first.NextCursor))
	var second subtitlePage
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &second) != nil || len(second.Jobs) != 1 || second.Jobs[0].ID != "same-a" {
		t.Fatalf("pagination changed after new source: %s", rr.Body.String())
	}
	snapshot.BootID = "next-boot"
	rr = phase2Get(t, h, cookie, "/admin/api/v1/subtitles?cursor="+url.QueryEscape(first.NextCursor))
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "stale_cursor") {
		t.Fatal("stale process cursor accepted")
	}
	for _, query := range []string{"limit=0", "limit=33", "limit=no", "cursor=invalid", "cursor=" + strings.Repeat("x", 1025)} {
		rr = phase2Get(t, h, cookie, "/admin/api/v1/subtitles?"+query)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("invalid query accepted: %s (%d)", query, rr.Code)
		}
	}
}

func TestSubtitleObservationHasBoundedDensePages(t *testing.T) {
	r := telemetry.New(nil)
	at := time.Now().UTC()
	snapshot := subtitles.Snapshot{Enabled: true, BootID: r.BootID()}
	for job := range 64 {
		v := subtitles.JobView{ID: fmt.Sprintf("job-%02d", job), CreatedAt: at.Add(time.Duration(job) * time.Second), SourceName: strings.Repeat("字幕<", 200)}
		for index := range 64 {
			v.Tracks = append(v.Tracks, subtitles.TrackView{Index: index, Name: strings.Repeat("<", 320), Language: strings.Repeat("<", 160), State: "unavailable", Reason: "missing_index"})
		}
		snapshot.Jobs = append(snapshot.Jobs, v)
	}
	r.SetSubtitlesProvider(func() subtitles.Snapshot { return snapshot })
	h, cookie := buildPhase2Handler(t, r, nil)
	rr := phase2Get(t, h, cookie, "/admin/api/v1/subtitles?limit=32")
	var page subtitlePage
	if rr.Code != 200 || rr.Body.Len() > subtitleMaxJSON || json.Unmarshal(rr.Body.Bytes(), &page) != nil || len(page.Jobs) == 0 || len(page.Jobs) >= 32 || page.NextCursor == "" || page.TotalJobs != 64 {
		t.Fatalf("unbounded dense page: status=%d bytes=%d body=%s", rr.Code, rr.Body.Len(), rr.Body.String())
	}
	if len(page.Jobs[0].Tracks) != 64 || len(snapshot.Jobs[0].Tracks[0].Name) != 320 {
		t.Fatal("observation dropped tracks or mutated the provider's data")
	}
}
