package adminapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminauth"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminmedia"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminquery"
	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
)

func recordSession(t *testing.T, app core.App, h http.Handler) *http.Cookie {
	t.Helper()
	su := createSuperuser(t, app, "records@example.test", "SuperSecret1!")
	token, err := su.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"token": token})
	rr := httptest.NewRecorder()
	req := withSameOrigin(httptest.NewRequest(http.MethodPost, "/admin/api/v1/session", bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	for _, c := range rr.Result().Cookies() {
		if c.Name == adminauth.CookieDev || c.Name == adminauth.CookieSecure {
			return c
		}
	}
	t.Fatal("session cookie not issued")
	return nil
}

func TestAuditFilteredAPIAndDetail(t *testing.T) {
	app := newTestApp(t)
	h := buildHandler(t, app, adminauth.NewStore(20))
	cookie := recordSession(t, app, h)
	col, err := app.FindCollectionByNameOrId("audit_logs")
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(col)
	r.Set("event", "proxy_media_upstream_failed")
	r.Set("error_kind", "upstream_unexpected_eof")
	r.Set("status", 206)
	r.Set("response_committed", true)
	r.Set("direction", "upstream")
	r.Set("message", "upstream failed token=never-expose")
	r.Set("path", "/emby/Videos/123/stream?api_key=never-expose")
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	q := url.Values{"view": {"errors"}, "direction": {"upstream"}, "from": {time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)}, "to": {time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)}}
	rr := phase2Get(t, h, cookie, "/admin/api/v1/audit?"+q.Encode())
	if rr.Code != 200 {
		t.Fatalf("audit: %d %s", rr.Code, rr.Body.String())
	}
	var page adminquery.AuditPage
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || !page.Items[0].IsError || !page.Items[0].ResponseCommitted {
		t.Fatalf("missing interrupted transfer: %+v", page)
	}
	rr = phase2Get(t, h, cookie, "/admin/api/v1/audit/"+r.Id)
	if rr.Code != 200 || strings.Contains(rr.Body.String(), "never-expose") {
		t.Fatalf("detail: %d %s", rr.Code, rr.Body.String())
	}
	rr = phase2Get(t, h, cookie, "/admin/api/v1/audit/missing")
	if rr.Code != 404 {
		t.Fatalf("missing detail: %d", rr.Code)
	}
	q.Set("direction", "arbitrary")
	rr = phase2Get(t, h, cookie, "/admin/api/v1/audit?"+q.Encode())
	if rr.Code != 400 {
		t.Fatalf("invalid filter: %d", rr.Code)
	}
}

func TestUserMediaAPIUnknownUserAndReadOnlyState(t *testing.T) {
	app := newTestApp(t)
	h := buildHandler(t, app, adminauth.NewStore(20))
	cookie := recordSession(t, app, h)
	col, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	u := core.NewRecord(col)
	u.SetEmail("viewer@example.test")
	u.SetPassword("Password1234!")
	u.Set("username", "viewer")
	u.Set("synthetic_user_id", "viewer-synthetic")
	u.Set("enabled", true)
	if err := app.Save(u); err != nil {
		t.Fatal(err)
	}
	col, err = app.FindCollectionByNameOrId("user_item_data")
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(col)
	r.Set("gateway_user", u.Id)
	r.Set("item_id", "123")
	r.Set("item_name", "Local title")
	r.Set("playback_position_ticks", 123000000)
	r.Set("run_time_ticks", 600000000)
	r.Set("is_favorite", true)
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	before := r.GetString("updated")
	rr := phase2Get(t, h, cookie, "/admin/api/v1/users/"+u.Id+"/media?view=resume")
	if rr.Code != 200 {
		t.Fatalf("user media: %d %s", rr.Code, rr.Body.String())
	}
	var page adminquery.UserMediaPage
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].PositionTicks != 123000000 || !page.Items[0].IsFavorite {
		t.Fatalf("wrong local state: %+v", page)
	}
	after, err := app.FindRecordById("user_item_data", r.Id)
	if err != nil {
		t.Fatal(err)
	}
	if after.GetString("updated") != before {
		t.Fatal("read mutated user data")
	}
	rr = phase2Get(t, h, cookie, "/admin/api/v1/users/missing/media")
	if rr.Code != 404 {
		t.Fatalf("missing user: %d", rr.Code)
	}
	rr = phase2Get(t, h, cookie, "/admin/api/v1/users/"+u.Id+"/media?view=wrong")
	if rr.Code != 400 {
		t.Fatalf("invalid view: %d", rr.Code)
	}
}

func TestUserMediaDoesNotAssociateReusedItemID(t *testing.T) {
	app := newTestApp(t)
	srv, err := New(Config{App: app, Media: adminmedia.New(adminMediaReaderStub{}), MatchMediaIdentity: gateway.MediaFingerprintMatches})
	if err != nil {
		t.Fatal(err)
	}
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	srv.Mount(router)
	h, err := router.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	cookie := recordSession(t, app, h)
	col, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	u := core.NewRecord(col)
	u.SetEmail("identity@example.test")
	u.SetPassword("Password1234!")
	u.Set("username", "identity")
	u.Set("synthetic_user_id", "identity-synthetic")
	u.Set("enabled", true)
	if err := app.Save(u); err != nil {
		t.Fatal(err)
	}
	col, err = app.FindCollectionByNameOrId("user_item_data")
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(col)
	r.Set("gateway_user", u.Id)
	r.Set("item_id", "a")
	r.Set("item_name", "Old resource")
	r.Set("fingerprint", "name=Old resource")
	r.Set("playback_position_ticks", 10000000)
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	for _, compatible := range []bool{false, true} {
		if compatible {
			r.Set("fingerprint", "name=Example episode")
			if err := app.Save(r); err != nil {
				t.Fatal(err)
			}
		}
		before := r.GetString("updated")
		rr := phase2Get(t, h, cookie, "/admin/api/v1/users/"+u.Id+"/media?view=resume")
		if rr.Code != 200 {
			t.Fatalf("user media: %d %s", rr.Code, rr.Body.String())
		}
		var page adminquery.UserMediaPage
		if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 {
			t.Fatalf("lost saved local state: %+v", page)
		}
		row := page.Items[0]
		if compatible && row.SourceRef != "source-a" {
			t.Fatalf("compatible identity not linked: %+v", row)
		}
		if !compatible && (row.SourceRef != "" || row.MetadataStatus != "identity_mismatch") {
			t.Fatalf("reused ID incorrectly linked: %+v", row)
		}
		if strings.Contains(rr.Body.String(), "fingerprint") {
			t.Fatal("internal fingerprint leaked")
		}
		after, err := app.FindRecordById("user_item_data", r.Id)
		if err != nil {
			t.Fatal(err)
		}
		if after.GetString("updated") != before || !after.GetDateTime("orphaned_at").IsZero() {
			t.Fatal("Admin read repaired/mutated local state")
		}
	}
}
