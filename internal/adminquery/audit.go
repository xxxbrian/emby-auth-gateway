package adminquery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

type AuditFilter struct {
	From, To                                          time.Time
	Limit                                             int
	Cursor, View, Event, ErrorKind, Direction, UserID string
}

type AuditPage struct {
	Items      []AuditDTO `json:"items"`
	NextCursor string     `json:"next_cursor"`
	HasMore    bool       `json:"has_more"`
	From       time.Time  `json:"from"`
	To         time.Time  `json:"to"`
}

type recordCursor struct {
	Scope string `json:"scope"`
	Time  string `json:"time"`
	ID    string `json:"id"`
}

func encodeRecordCursor(scope string, record *core.Record, field string) string {
	b, _ := json.Marshal(recordCursor{Scope: scope, Time: record.GetString(field), ID: record.Id})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeRecordCursor(raw, scope string) (recordCursor, error) {
	if len(raw) > 1024 {
		return recordCursor{}, errors.New("invalid cursor")
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	var c recordCursor
	if err != nil || json.Unmarshal(b, &c) != nil || c.Scope != scope || c.Time == "" || len(c.Time) > 40 || c.ID == "" || len(c.ID) > 80 {
		return recordCursor{}, errors.New("invalid cursor")
	}
	return c, nil
}

// queryRecords keeps its concurrency slot until PocketBase finishes, even if
// the caller's deadline expires. No abandoned query can free a slot early.
func (q *Querier) queryRecords(ctx context.Context, collection, filter, sort string, limit int, params dbx.Params) ([]*core.Record, error) {
	ctx, cancel := context.WithTimeout(ctx, AuditQueryTimeout)
	defer cancel()
	if err := q.acquire(ctx); err != nil {
		return nil, err
	}
	type result struct {
		records []*core.Record
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		defer q.release()
		records, err := q.app.FindRecordsByFilter(collection, filter, sort, limit, 0, params)
		ch <- result{records, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.records, r.err
	}
}

var auditFailureEvents = []string{
	"login_failure", "logout_failure", "session_save_failure", "backend_auth_failure",
	"backend_token_refresh_failed", "backend_token_refresh_failure", "proxy_backend_unavailable",
	"proxy_read_failed", "proxy_invalid_image", "proxy_media_upstream_failed", "proxy_media_downstream_timeout",
	"userdata_write_failed", "userdata_write_failure", "overlay_failed", "overlay_failure",
}

func (q *Querier) ListAuditPage(ctx context.Context, f AuditFilter) (AuditPage, error) {
	p := AuditPage{Items: []AuditDTO{}, From: f.From.UTC(), To: f.To.UTC()}
	if f.From.IsZero() || f.To.IsZero() || !f.To.After(f.From) || f.To.Sub(f.From) > MaxAuditWindow {
		return p, errors.New("audit window must be positive and <= 24h")
	}
	if f.Limit <= 0 || f.Limit > MaxAuditLimit {
		f.Limit = MaxAuditLimit
	}
	if f.View != "" && f.View != "audit" && f.View != "errors" {
		return p, errors.New("invalid audit view")
	}
	if f.Direction != "" && f.Direction != "upstream" && f.Direction != "downstream" {
		return p, errors.New("invalid direction")
	}
	filter := "created >= {:from} && created <= {:to}"
	params := dbx.Params{"from": p.From, "to": p.To}
	if f.View == "errors" {
		parts := []string{"error_kind != ''", "status >= 400"}
		for i, event := range auditFailureEvents {
			key := fmt.Sprintf("failure%d", i)
			parts = append(parts, "event = {:"+key+"}")
			params[key] = event
		}
		filter += " && (" + strings.Join(parts, " || ") + ") && status != 499 && error_kind != 'client_canceled' && error_kind != 'canceled'"
	}
	for _, v := range []struct{ field, value string }{{"event", f.Event}, {"error_kind", f.ErrorKind}, {"direction", f.Direction}, {"gateway_user", f.UserID}} {
		if len(v.value) > 255 {
			return p, errors.New("filter is too long")
		}
		if v.value != "" {
			filter += " && " + v.field + " = {:" + v.field + "}"
			params[v.field] = v.value
		}
	}
	if raw := strings.TrimSpace(f.Cursor); raw != "" {
		// Legacy timestamp cursors retain their previous strictly-before behavior.
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			filter += " && created < {:cursor_time}"
			params["cursor_time"] = t.UTC()
		} else {
			c, err := decodeRecordCursor(raw, "audit")
			if err != nil {
				return p, err
			}
			filter += " && (created < {:cursor_time} || (created = {:cursor_time} && id < {:cursor_id}))"
			params["cursor_time"], params["cursor_id"] = c.Time, c.ID
		}
	}
	records, err := q.queryRecords(ctx, "audit_logs", filter, "-created,-id", f.Limit+1, params)
	if err != nil {
		return p, err
	}
	p.HasMore = len(records) > f.Limit
	if p.HasMore {
		records = records[:f.Limit]
	}
	for _, r := range records {
		p.Items = append(p.Items, auditFromRecord(r))
	}
	if p.HasMore {
		p.NextCursor = encodeRecordCursor("audit", records[len(records)-1], "created")
	}
	return p, nil
}

func (q *Querier) GetAudit(ctx context.Context, id string) (AuditDTO, error) {
	if len(id) > 80 || id == "" {
		return AuditDTO{}, errors.New("invalid audit id")
	}
	records, err := q.queryRecords(ctx, "audit_logs", "id = {:id}", "", 1, dbx.Params{"id": id})
	if err != nil {
		return AuditDTO{}, err
	}
	if len(records) == 0 {
		return AuditDTO{}, ErrRecordNotFound
	}
	return auditFromRecord(records[0]), nil
}

var ErrRecordNotFound = errors.New("record not found")

var auditURL = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)
var auditBearer = regexp.MustCompile(`(?i)\bBearer\s+[^\s,;]+`)
var auditSecret = regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|refresh[_-]?token|token|password|authorization|cookie|secret)(?:\\?["'])?\s*[:=]\s*(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s,;}\]]+)`)

func cleanAuditText(s string, limit int) string {
	s = auditURL.ReplaceAllString(s, "[redacted URL]")
	s = auditBearer.ReplaceAllString(s, "Bearer [redacted]")
	s = auditSecret.ReplaceAllString(s, "$1=[redacted]")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	r := []rune(strings.TrimSpace(s))
	if len(r) > limit {
		r = r[:limit]
	}
	return string(r)
}

func sanitizeAudit(v AuditDTO) AuditDTO {
	v.Message = cleanAuditText(v.Message, 2048)
	v.Event = cleanAuditText(v.Event, 255)
	v.ErrorKind = cleanAuditText(v.ErrorKind, 80)
	if u, err := url.Parse(v.Path); err == nil {
		v.Path = u.EscapedPath()
	} else {
		v.Path = "[unavailable]"
	}
	v.Path = cleanAuditText(v.Path, 512)
	v.Severity = "info"
	failure := v.ErrorKind != "" || v.Status >= 400
	for _, event := range auditFailureEvents {
		failure = failure || v.Event == event
	}
	if v.Status == 499 || v.ErrorKind == "canceled" || v.ErrorKind == "client_canceled" {
		failure = false
	}
	v.IsError = failure
	if failure {
		v.Severity = "error"
		if v.Status >= 400 && v.Status < 500 && v.ErrorKind == "" {
			v.Severity = "warning"
		}
	}
	return v
}
