package adminquery

import (
	"context"
	"errors"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

type UserMediaItem struct {
	ID             string     `json:"id"`
	ItemID         string     `json:"item_id"`
	ItemName       string     `json:"item_name"`
	ItemType       string     `json:"item_type"`
	SeriesName     string     `json:"series_name"`
	SeasonNumber   *int       `json:"season_number"`
	EpisodeNumber  *int       `json:"episode_number"`
	RuntimeTicks   int64      `json:"runtime_ticks"`
	PositionTicks  int64      `json:"position_ticks"`
	Played         bool       `json:"played"`
	IsFavorite     bool       `json:"is_favorite"`
	LastPlayedAt   *time.Time `json:"last_played_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	Orphaned       bool       `json:"orphaned"`
	SourceRef      string     `json:"source_ref,omitempty"`
	Fingerprint    string     `json:"-"`
	MetadataStatus string     `json:"metadata_status,omitempty"`
}

type UserMediaPage struct {
	Items      []UserMediaItem `json:"items"`
	NextCursor string          `json:"next_cursor"`
	HasMore    bool            `json:"has_more"`
	View       string          `json:"view"`
}

func (q *Querier) ListUserMedia(ctx context.Context, userID, view, cursor string, limit int) (UserMediaPage, error) {
	p := UserMediaPage{Items: []UserMediaItem{}, View: view}
	if view == "" {
		view = "recent"
		p.View = view
	}
	if view != "recent" && view != "resume" && view != "favorites" {
		return p, errors.New("invalid media view")
	}
	if userID == "" || len(userID) > 80 {
		return p, errors.New("invalid user id")
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	users, err := q.queryRecords(ctx, "users", "id = {:user}", "", 1, dbx.Params{"user": userID})
	if err != nil {
		return p, err
	}
	if len(users) == 0 {
		return p, ErrRecordNotFound
	}
	filter := "gateway_user = {:user}"
	params := dbx.Params{"user": userID}
	field := "updated"
	switch view {
	case "recent":
		filter += " && last_played_date != ''"
		field = "last_played_date"
	case "resume":
		filter += " && playback_position_ticks > 0 && played = false && hide_from_resume = false && orphaned_at = ''"
	case "favorites":
		filter += " && is_favorite = true"
	}
	scope := "media:" + userID + ":" + view
	if cursor != "" {
		c, err := decodeRecordCursor(cursor, scope)
		if err != nil {
			return p, err
		}
		filter += " && (" + field + " < {:cursor_time} || (" + field + " = {:cursor_time} && id < {:cursor_id}))"
		params["cursor_time"], params["cursor_id"] = c.Time, c.ID
	}
	records, err := q.queryRecords(ctx, "user_item_data", filter, "-"+field+",-id", limit+1, params)
	if err != nil {
		return p, err
	}
	p.HasMore = len(records) > limit
	if p.HasMore {
		records = records[:limit]
	}
	for _, r := range records {
		p.Items = append(p.Items, userMediaFromRecord(r))
	}
	if p.HasMore {
		p.NextCursor = encodeRecordCursor(scope, records[len(records)-1], field)
	}
	return p, nil
}

func userMediaFromRecord(r *core.Record) UserMediaItem {
	v := UserMediaItem{ID: r.Id, ItemID: r.GetString("item_id"), ItemName: r.GetString("item_name"), ItemType: r.GetString("item_type"), SeriesName: r.GetString("series_name"), RuntimeTicks: int64(r.GetInt("run_time_ticks")), PositionTicks: int64(r.GetInt("playback_position_ticks")), Played: r.GetBool("played"), IsFavorite: r.GetBool("is_favorite"), UpdatedAt: r.GetDateTime("updated").Time().UTC(), Orphaned: !r.GetDateTime("orphaned_at").IsZero()}
	if t := r.GetDateTime("last_played_date"); !t.IsZero() {
		at := t.Time().UTC()
		v.LastPlayedAt = &at
	}
	v.Fingerprint = r.GetString("fingerprint")
	// Legacy numeric fields have no presence bits: zero is ambiguous, so leave
	// it absent. Fresh upstream metadata can correctly identify specials/S00.
	if n := r.GetInt("index_number"); n > 0 {
		v.EpisodeNumber = &n
	}
	if n := r.GetInt("parent_index_number"); n > 0 {
		v.SeasonNumber = &n
	}
	return v
}
