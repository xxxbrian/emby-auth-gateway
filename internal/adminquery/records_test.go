package adminquery

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

func seedAuditRecord(t *testing.T, app core.App, event string, status int, kind string, at time.Time) string {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("audit_logs")
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(col)
	r.Set("event", event)
	r.Set("status", status)
	r.Set("error_kind", kind)
	r.Set("message", `request https://secret.example/media?api_key=never-show failed; password="never-show" Authorization: Bearer never-show`)
	r.Set("path", "/emby/Videos/123/stream?api_key=never-show")
	r.Set("response_committed", status == 206)
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	_, err = app.DB().NewQuery("UPDATE audit_logs SET created={:at} WHERE id={:id}").Bind(dbx.Params{"at": at.UTC().Format("2006-01-02 15:04:05.000Z"), "id": r.Id}).Execute()
	if err != nil {
		t.Fatal(err)
	}
	return r.Id
}

func TestAuditErrorsFilterBeforePagingAndStableCursor(t *testing.T) {
	app := newTestApp(t)
	q := New(app, 2)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for range 6 {
		seedAuditRecord(t, app, "login_success", 200, "", now.Add(-time.Minute))
	}
	ids := map[string]bool{}
	for range 3 {
		id := seedAuditRecord(t, app, "proxy_media_upstream_failed", 206, "upstream_unexpected_eof", now.Add(-2*time.Minute))
		ids[id] = true
	}
	seedAuditRecord(t, app, "proxy_canceled", 499, "canceled", now.Add(-3*time.Minute))
	f := AuditFilter{From: now.Add(-time.Hour), To: now, Limit: 1, View: "errors"}
	for len(ids) > 0 {
		p, err := q.ListAuditPage(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Items) != 1 || !ids[p.Items[0].ID] {
			t.Fatalf("unexpected page %+v", p)
		}
		row := p.Items[0]
		delete(ids, row.ID)
		if !row.IsError || row.Severity != "error" || !row.ResponseCommitted {
			t.Fatalf("failure not explained: %+v", row)
		}
		if strings.Contains(row.Message, "never-show") || strings.Contains(row.Path, "?") {
			t.Fatalf("secret in redacted row: %+v", row)
		}
		if (len(ids) > 0) != p.HasMore {
			t.Fatalf("has_more=%v remaining=%d", p.HasMore, len(ids))
		}
		f.Cursor = p.NextCursor
	}
}

func TestAuditDetailsRedactionAndLegacyCursor(t *testing.T) {
	app := newTestApp(t)
	q := New(app, 1)
	now := time.Now().UTC().Truncate(time.Second)
	id := seedAuditRecord(t, app, "proxy_backend_unavailable", 504, "upstream_timeout", now.Add(-time.Minute))
	row, err := q.GetAudit(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.Message, "never-show") || row.Path != "/emby/Videos/123/stream" {
		t.Fatalf("unsafe detail: %+v", row)
	}
	p, err := q.ListAuditPage(context.Background(), AuditFilter{From: now.Add(-time.Hour), To: now, Cursor: now.Format(time.RFC3339Nano)})
	if err != nil || len(p.Items) != 1 {
		t.Fatalf("legacy cursor: %+v %v", p, err)
	}
	_, err = q.ListAuditPage(context.Background(), AuditFilter{From: now.Add(-time.Hour), To: now, Cursor: "malformed"})
	if err == nil {
		t.Fatal("accepted invalid cursor")
	}
}

func TestAuditRedactsJSONCredentials(t *testing.T) {
	for _, raw := range []string{
		`{"AccessToken":"secret-value","Password":"secret-password","Authorization":"Bearer secret-bearer"}`,
		`{"Password":"escaped\"secret-tail"}`,
		`upstream returned {"api_key": "secret-value", "cookie": "session=secret-cookie"}`,
	} {
		got := cleanAuditText(raw, 2048)
		if strings.Contains(got, "secret-") || strings.Contains(got, "secret-tail") {
			t.Fatalf("credential survived: %s", got)
		}
	}
}

func seedMediaUser(t *testing.T, app core.App, name string) string {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(col)
	r.SetEmail(name + "@example.test")
	r.Set("username", name)
	r.Set("synthetic_user_id", name+"-synthetic")
	r.Set("enabled", true)
	r.SetPassword("Password1234!")
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r.Id
}

func seedUserMedia(t *testing.T, app core.App, user, id string, position int64, played, favorite, hidden bool) {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("user_item_data")
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(col)
	r.Set("gateway_user", user)
	r.Set("item_id", id)
	r.Set("playback_position_ticks", position)
	r.Set("run_time_ticks", int64(9000000000))
	r.Set("played", played)
	r.Set("is_favorite", favorite)
	r.Set("hide_from_resume", hidden)
	r.Set("last_played_date", time.Now().Add(-time.Minute))
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
}

func TestUserMediaIsLocalIsolatedAndFiltered(t *testing.T) {
	app := newTestApp(t)
	q := New(app, 2)
	ctx := context.Background()
	a := seedMediaUser(t, app, "alice")
	b := seedMediaUser(t, app, "bob")
	seedUserMedia(t, app, a, "shared", 10000000, false, true, false)
	seedUserMedia(t, app, b, "shared", 80000000, true, false, false)
	seedUserMedia(t, app, a, "hidden", 10000000, false, false, true)
	seedUserMedia(t, app, a, "finished", 10000000, true, false, false)
	p, err := q.ListUserMedia(ctx, a, "resume", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 1 || p.Items[0].ItemID != "shared" || p.Items[0].PositionTicks != 10000000 || p.Items[0].Played {
		t.Fatalf("incorrect local resume: %+v", p)
	}
	p, err = q.ListUserMedia(ctx, b, "resume", "", 50)
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("bob resume: %+v %v", p, err)
	}
	p, err = q.ListUserMedia(ctx, a, "favorites", "", 50)
	if err != nil || len(p.Items) != 1 {
		t.Fatalf("favorites: %+v %v", p, err)
	}
	p, err = q.ListUserMedia(ctx, a, "recent", "", 1)
	if err != nil || !p.HasMore || p.NextCursor == "" {
		t.Fatalf("recent page: %+v %v", p, err)
	}
	next, err := q.ListUserMedia(ctx, a, "recent", p.NextCursor, 1)
	if err != nil || len(next.Items) != 1 || next.Items[0].ID == p.Items[0].ID {
		t.Fatalf("next page: %+v %v", next, err)
	}
	if _, err = q.ListUserMedia(ctx, b, "recent", p.NextCursor, 1); err == nil {
		t.Fatal("accepted cursor from another user")
	}
}
