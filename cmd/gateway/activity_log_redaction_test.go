package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/logger"
)

func TestRedactRawQueryForActivityLog(t *testing.T) {
	t.Parallel()

	const secret = "s3cret-value"
	const other = "keep-me"

	cases := []struct {
		name string
		in   string
		// wantContains must all appear in the redacted query.
		wantContains []string
		// wantNotContains must not appear.
		wantNotContains []string
		// wantExact, when non-empty, requires exact equality.
		wantExact string
	}{
		{
			name:            "case insensitive api_key",
			in:              "API_KEY=" + secret + "&foo=" + other,
			wantContains:    []string{"API_KEY=" + activityLogRedactedValue, "foo=" + other},
			wantNotContains: []string{secret},
		},
		{
			name:            "access_token",
			in:              "access_token=" + secret,
			wantContains:    []string{"access_token=" + activityLogRedactedValue},
			wantNotContains: []string{secret},
		},
		{
			name:            "generic token",
			in:              "token=" + secret + "&q=1",
			wantContains:    []string{"token=" + activityLogRedactedValue, "q=1"},
			wantNotContains: []string{secret},
		},
		{
			name:            "X-Emby-Token case variants",
			in:              "X-Emby-Token=" + secret + "&x-emby-token=" + secret,
			wantContains:    []string{activityLogRedactedValue},
			wantNotContains: []string{secret},
		},
		{
			name:            "X-MediaBrowser-Token",
			in:              "X-MediaBrowser-Token=" + secret,
			wantContains:    []string{"X-MediaBrowser-Token=" + activityLogRedactedValue},
			wantNotContains: []string{secret},
		},
		{
			name:            "duplicate sensitive values",
			in:              "token=a&token=b&token=",
			wantContains:    []string{"token=" + activityLogRedactedValue},
			wantNotContains: []string{"token=a", "token=b"},
		},
		{
			name:            "encoded sensitive value",
			in:              "api_key=" + url.QueryEscape(secret+" has spaces") + "&ok=1",
			wantContains:    []string{"api_key=" + activityLogRedactedValue, "ok=1"},
			wantNotContains: []string{secret, "has+spaces", "has%20spaces"},
		},
		{
			name:         "non-sensitive query preserved",
			in:           "foo=bar&baz=qux",
			wantContains: []string{"foo=bar", "baz=qux"},
		},
		{
			name:            "empty sensitive value",
			in:              "api_key=&keep=1",
			wantContains:    []string{"api_key=" + activityLogRedactedValue, "keep=1"},
			wantNotContains: []string{"api_key=&"},
		},
		{
			name:      "empty query",
			in:        "",
			wantExact: "",
		},
		{
			name:            "malformed semicolon query hidden entirely",
			in:              "api_key=" + secret + ";token=" + secret,
			wantExact:       activityLogRedactedValue,
			wantNotContains: []string{secret},
		},
		{
			name:            "malformed percent escape hidden entirely",
			in:              "api_key=" + secret + "&bad=%zz",
			wantExact:       activityLogRedactedValue,
			wantNotContains: []string{secret},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := redactRawQueryForActivityLog(tc.in, "")
			if tc.wantExact != "" || tc.in == "" {
				if got != tc.wantExact {
					t.Fatalf("got %q, want exact %q", got, tc.wantExact)
				}
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Fatalf("got %q, missing %q", got, want)
				}
			}
			for _, ban := range tc.wantNotContains {
				if ban != "" && strings.Contains(got, ban) {
					t.Fatalf("got %q, unexpectedly contains banned fragment", got)
				}
			}
			// Duplicate sensitive keys must all be redacted when parse succeeds.
			if strings.Count(tc.in, "token=") >= 2 && !strings.Contains(tc.in, ";") && !strings.Contains(tc.in, "%zz") {
				values, err := url.ParseQuery(got)
				if err != nil {
					t.Fatalf("parse redacted query: %v", err)
				}
				for _, v := range values["token"] {
					if v != activityLogRedactedValue {
						t.Fatalf("token value %q, want %s", v, activityLogRedactedValue)
					}
				}
				if len(values["token"]) < 2 {
					t.Fatalf("expected duplicate token keys preserved, got %v", values["token"])
				}
			}
		})
	}
}

func TestRedactURLForActivityLogRemovesUserinfo(t *testing.T) {
	t.Parallel()
	const secret = "userinfo-secret"
	u, err := url.Parse("https://alice:" + secret + "@example.com/emby/Items?api_key=" + secret + "&keep=1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	redactURLForActivityLog(u, "")
	if u.User != nil {
		t.Fatalf("userinfo still present: %v", u.User)
	}
	if strings.Contains(u.String(), secret) {
		t.Fatalf("redacted url still contains secret material")
	}
	if !strings.Contains(u.RawQuery, "api_key="+activityLogRedactedValue) {
		t.Fatalf("raw query = %q, want redacted api_key", u.RawQuery)
	}
	if !strings.Contains(u.RawQuery, "keep=1") {
		t.Fatalf("raw query = %q, want keep=1 preserved", u.RawQuery)
	}
}

func TestRedactRawQuerySelectedTokenUnderArbitraryKey(t *testing.T) {
	t.Parallel()
	const selected = "selected-gateway-credential-value"
	const unrelated = "unrelated-43-char-looking-but-not-selected-xx"
	got := redactRawQueryForActivityLog("signature="+selected+"&keep=1&other="+unrelated, selected)
	if strings.Contains(got, selected) {
		t.Fatalf("selected token leaked: %q", got)
	}
	if !strings.Contains(got, "signature="+activityLogRedactedValue) {
		t.Fatalf("signature not redacted: %q", got)
	}
	if !strings.Contains(got, "keep=1") || !strings.Contains(got, "other="+unrelated) {
		t.Fatalf("unrelated values not preserved: %q", got)
	}
}

func TestRedactRefererForActivityLog(t *testing.T) {
	t.Parallel()
	const secret = "referer-secret"

	cases := []struct {
		name            string
		in              string
		selected        string
		wantContains    []string
		wantNotContains []string
		wantExact       string
	}{
		{
			name:            "redacts sensitive query and strips userinfo",
			in:              "https://bob:" + secret + "@example.com/web?access_token=" + secret + "&x=1",
			wantContains:    []string{"https://example.com/web?", "access_token=" + activityLogRedactedValue, "x=1"},
			wantNotContains: []string{secret, "bob:"},
		},
		{
			name:            "redacts selected token under arbitrary key",
			in:              "https://client.example/app?signature=" + secret + "&page=1",
			selected:        secret,
			wantContains:    []string{"signature=" + activityLogRedactedValue, "page=1"},
			wantNotContains: []string{secret},
		},
		{
			name:      "empty",
			in:        "",
			wantExact: "",
		},
		{
			name:            "malformed referer hidden",
			in:              "://bad host with " + secret,
			wantExact:       activityLogRedactedValue,
			wantNotContains: []string{secret},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := redactRefererForActivityLog(tc.in, tc.selected)
			if tc.wantExact != "" || tc.in == "" {
				if got != tc.wantExact {
					t.Fatalf("got %q, want exact %q", got, tc.wantExact)
				}
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Fatalf("got %q, missing %q", got, want)
				}
			}
			for _, ban := range tc.wantNotContains {
				if ban != "" && strings.Contains(got, ban) {
					t.Fatalf("got %q, unexpectedly contains banned fragment", got)
				}
			}
		})
	}
}

func TestActivityLogTokenRedactionMiddlewarePriority(t *testing.T) {
	t.Parallel()
	h := activityLogTokenRedactionMiddleware()
	if h.Id != activityLogTokenRedactionMiddlewareId {
		t.Fatalf("id = %q, want %q", h.Id, activityLogTokenRedactionMiddlewareId)
	}
	if h.Priority <= apis.DefaultActivityLoggerMiddlewarePriority {
		t.Fatalf("priority %d must be > activity logger priority %d", h.Priority, apis.DefaultActivityLoggerMiddlewarePriority)
	}
}

func TestActivityLogTokenRedactionIntegration(t *testing.T) {
	// Product-level: handlers still see original query tokens; PB _logs store redacted URL/referer.
	const secret = "integration-activity-log-secret-value"
	const nonSensitive = "visible-param"

	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "test_activity_log_redaction",
	})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	defer app.Cleanup()

	// TestApp disables request logs by default; enable retention for this test.
	app.Settings().Logs.MaxDays = 1
	app.Settings().Logs.MinLevel = int(0) // slog.LevelInfo

	pbRouter, err := apis.NewRouter(app)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	pbRouter.Bind(activityLogTokenRedactionMiddleware())

	var (
		handlerSawAPIKey  string
		handlerSawToken   string
		handlerSawHeader  string
		handlerSawReferer string
		handlerSawKeep    string
	)
	pbRouter.GET("/gateway-activity-probe", func(e *core.RequestEvent) error {
		handlerSawAPIKey = e.Request.URL.Query().Get("api_key")
		handlerSawToken = e.Request.URL.Query().Get("token")
		handlerSawHeader = e.Request.Header.Get("X-Emby-Token")
		handlerSawReferer = e.Request.Referer()
		handlerSawKeep = e.Request.URL.Query().Get("keep")
		return e.JSON(http.StatusOK, map[string]any{"ok": true})
	})

	mux, err := pbRouter.BuildMux()
	if err != nil {
		t.Fatalf("build mux: %v", err)
	}

	reqURL := "/gateway-activity-probe?api_key=" + url.QueryEscape(secret) +
		"&token=" + url.QueryEscape(secret) +
		"&signature=" + url.QueryEscape(secret) +
		"&keep=" + url.QueryEscape(nonSensitive)
	req := httptest.NewRequest(http.MethodGet, reqURL, nil)
	req.Header.Set("Referer", "https://client.example/app?signature="+url.QueryEscape(secret)+"&page=1")
	req.Header.Set("X-Emby-Token", secret)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if handlerSawAPIKey != secret || handlerSawToken != secret {
		t.Fatal("handler did not observe original query tokens")
	}
	if handlerSawHeader != secret {
		t.Fatal("handler did not observe original X-Emby-Token header")
	}
	if !strings.Contains(handlerSawReferer, secret) {
		t.Fatal("handler did not observe original referer token")
	}
	if handlerSawKeep != nonSensitive {
		t.Fatalf("handler keep param = %q, want %q", handlerSawKeep, nonSensitive)
	}

	logs := waitFlushActivityLogs(t, app)
	if len(logs) == 0 {
		t.Fatal("expected at least one activity log entry")
	}

	foundRequestLog := false
	for _, entry := range logs {
		blob := activityLogEntryBlob(t, entry)
		if strings.Contains(blob, secret) {
			t.Fatal("activity log still contains secret material")
		}
		// Only assert REDACTED on request-shaped logs.
		dataURL, _ := entry.Data["url"].(string)
		dataReferer, _ := entry.Data["referer"].(string)
		if dataURL == "" && dataReferer == "" && !strings.Contains(entry.Message, "/gateway-activity-probe") {
			continue
		}
		foundRequestLog = true
		if strings.Contains(entry.Message, secret) || strings.Contains(dataURL, secret) || strings.Contains(dataReferer, secret) {
			t.Fatal("request activity log fields still contain secret material")
		}
		if !strings.Contains(entry.Message, activityLogRedactedValue) &&
			!strings.Contains(dataURL, activityLogRedactedValue) {
			t.Fatal("expected REDACTED marker in activity log url/message")
		}
		if dataReferer != "" && !strings.Contains(dataReferer, activityLogRedactedValue) {
			t.Fatal("expected REDACTED marker in activity log referer")
		}
		if dataURL != "" && !strings.Contains(dataURL, "keep="+nonSensitive) {
			t.Fatalf("expected non-sensitive query preserved in log url")
		}
		if dataURL != "" && strings.Contains(dataURL, "signature="+secret) {
			t.Fatal("selected token under signature key leaked in activity log url")
		}
	}
	if !foundRequestLog {
		t.Fatal("did not find request activity log entry")
	}
}

func waitFlushActivityLogs(t *testing.T, app core.App) []*core.Log {
	t.Helper()
	handler, ok := app.Logger().Handler().(*logger.BatchHandler)
	if !ok {
		t.Fatal("expected logger.BatchHandler")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for {
		if err := handler.WriteAll(ctx); err != nil {
			t.Fatalf("flush batch handler: %v", err)
		}
		var logs []*core.Log
		if err := app.LogQuery().OrderBy("created DESC").All(&logs); err != nil {
			t.Fatalf("query logs: %v", err)
		}
		if len(logs) > 0 {
			return logs
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for activity logs to flush")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func activityLogEntryBlob(t *testing.T, entry *core.Log) string {
	t.Helper()
	raw, err := json.Marshal(entry.Data)
	if err != nil {
		t.Fatalf("marshal log data: %v", err)
	}
	return entry.Message + "\n" + string(raw)
}
