package pbschema

import "github.com/pocketbase/pocketbase/core"

func collections(ids map[string]string) []*core.Collection {
	return []*core.Collection{sessions(ids["users"]), audit(ids["users"]), playback(ids["users"]), userData(ids["users"]), childCounts(), preferences(ids["users"]), policies(), sources(), endpoints(ids["upstream_sources"])}
}
func base(name string) *core.Collection { c := core.NewBaseCollection(name); lock(c); return c }
func dates(c *core.Collection) {
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	c.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
}
func rel(name, id string, required bool) *core.RelationField {
	return &core.RelationField{Name: name, CollectionId: id, Required: required, MaxSelect: 1}
}

func sessions(uid string) *core.Collection {
	c := base("gateway_sessions")
	c.Fields.Add(&core.TextField{Name: "gateway_token_hash", Required: true, Max: 128})
	c.Fields.Add(rel("gateway_user", uid, true))
	c.Fields.Add(&core.TextField{Name: "gateway_username", Max: 255})
	c.Fields.Add(&core.TextField{Name: "synthetic_user_id", Required: true, Max: 80})
	for _, f := range []struct {
		n string
		m int
	}{{"client", 255}, {"device", 255}, {"device_id", 255}, {"version", 80}, {"remote_ip", 80}} {
		c.Fields.Add(&core.TextField{Name: f.n, Max: f.m})
	}
	c.Fields.Add(&core.DateField{Name: "expires_at", Required: true})
	c.Fields.Add(&core.DateField{Name: "revoked_at"})
	dates(c)
	c.AddIndex("idx_gateway_sessions_token_hash", true, "gateway_token_hash", "")
	return c
}
func audit(uid string) *core.Collection {
	c := base("audit_logs")
	c.Fields.Add(rel("gateway_user", uid, false))
	c.Fields.Add(&core.TextField{Name: "synthetic_user_id", Max: 80})
	for _, f := range []struct {
		n   string
		m   int
		req bool
	}{{"event", 255, true}, {"message", 0, false}, {"method", 32, false}, {"path", 512, false}} {
		c.Fields.Add(&core.TextField{Name: f.n, Max: f.m, Required: f.req})
	}
	c.Fields.Add(&core.NumberField{Name: "status", OnlyInt: true})
	c.Fields.Add(&core.TextField{Name: "remote_ip", Max: 80})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	c.Fields.Add(&core.TextField{Name: "error_kind", Max: 80})
	c.Fields.Add(&core.TextField{Name: "direction", Max: 32})
	for _, n := range []string{"bytes_transferred", "duration_ms", "upstream_status"} {
		c.Fields.Add(&core.NumberField{Name: n, OnlyInt: true})
	}
	c.Fields.Add(&core.BoolField{Name: "response_committed"})
	return c
}
func playback(uid string) *core.Collection {
	c := base("playback_events")
	c.Fields.Add(rel("gateway_user", uid, true))
	c.Fields.Add(&core.TextField{Name: "synthetic_user_id", Max: 80})
	c.Fields.Add(&core.TextField{Name: "item_id", Required: true, Max: 80})
	c.Fields.Add(&core.TextField{Name: "item_name", Max: 255})
	c.Fields.Add(&core.SelectField{Name: "event", Required: true, Values: []string{"playing", "progress", "stopped"}})
	c.Fields.Add(&core.NumberField{Name: "playback_position_ticks", OnlyInt: true})
	c.Fields.Add(&core.BoolField{Name: "played"})
	c.Fields.Add(&core.NumberField{Name: "played_percentage"})
	c.Fields.Add(&core.TextField{Name: "remote_ip", Max: 80})
	c.Fields.Add(&core.DateField{Name: "occurred_at", Required: true})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	c.AddIndex("idx_playback_events_gateway_item", false, "gateway_user, item_id", "")
	return c
}
func userData(uid string) *core.Collection {
	c := base("user_item_data")
	c.Fields.Add(rel("gateway_user", uid, true))
	c.Fields.Add(&core.TextField{Name: "synthetic_user_id", Max: 80})
	c.Fields.Add(&core.TextField{Name: "item_id", Required: true, Max: 80})
	c.Fields.Add(&core.TextField{Name: "item_name", Max: 255})
	c.Fields.Add(&core.TextField{Name: "item_type", Max: 80})
	c.Fields.Add(&core.TextField{Name: "series_id", Max: 80})
	c.Fields.Add(&core.TextField{Name: "series_name", Max: 255})
	c.Fields.Add(&core.NumberField{Name: "index_number", OnlyInt: true})
	c.Fields.Add(&core.NumberField{Name: "parent_index_number", OnlyInt: true})
	c.Fields.Add(&core.BoolField{Name: "played"})
	c.Fields.Add(&core.NumberField{Name: "playback_position_ticks", OnlyInt: true})
	c.Fields.Add(&core.NumberField{Name: "played_percentage"})
	c.Fields.Add(&core.BoolField{Name: "played_percentage_set"})
	c.Fields.Add(&core.DateField{Name: "last_played_date"})
	c.Fields.Add(&core.NumberField{Name: "play_count", OnlyInt: true})
	c.Fields.Add(&core.BoolField{Name: "is_favorite"})
	c.Fields.Add(&core.BoolField{Name: "likes"})
	c.Fields.Add(&core.BoolField{Name: "likes_set"})
	c.Fields.Add(&core.TextField{Name: "fingerprint", Max: 255})
	c.Fields.Add(&core.DateField{Name: "orphaned_at"})
	c.Fields.Add(&core.DateField{Name: "last_seen_at"})
	dates(c)
	c.Fields.Add(&core.TextField{Name: "season_id", Max: 80})
	c.Fields.Add(&core.NumberField{Name: "run_time_ticks", OnlyInt: true})
	c.AddIndex("idx_user_item_data_gateway_item", true, "gateway_user, item_id", "")
	c.AddIndex("idx_user_item_data_gateway_series", false, "gateway_user, series_id", "")
	c.AddIndex("idx_user_item_data_gateway_season", false, "gateway_user, season_id", "")
	return c
}
func childCounts() *core.Collection {
	c := base("item_child_counts")
	c.Fields.Add(&core.TextField{Name: "item_id", Required: true, Max: 80})
	c.Fields.Add(&core.NumberField{Name: "child_count", Required: true, OnlyInt: true})
	dates(c)
	c.AddIndex("idx_item_child_counts_item", true, "item_id", "")
	return c
}
func preferences(uid string) *core.Collection {
	c := base("display_preferences")
	c.Fields.Add(rel("gateway_user", uid, true))
	c.Fields.Add(&core.TextField{Name: "synthetic_user_id", Max: 80})
	c.Fields.Add(&core.TextField{Name: "preference_id", Required: true, Max: 255})
	c.Fields.Add(&core.TextField{Name: "client", Max: 255})
	c.Fields.Add(&core.TextField{Name: "payload_json", Required: true, Max: 1048576})
	dates(c)
	c.AddIndex("idx_display_preferences_gateway_pref_client", true, "gateway_user, preference_id, client", "")
	return c
}
func policies() *core.Collection {
	c := base("path_policies")
	c.Fields.Add(&core.TextField{Name: "method", Required: true, Max: 32})
	c.Fields.Add(&core.TextField{Name: "path", Required: true, Max: 512})
	c.Fields.Add(&core.SelectField{Name: "action", Required: true, Values: []string{"allow", "deny"}})
	c.Fields.Add(&core.NumberField{Name: "priority", OnlyInt: true})
	c.Fields.Add(&core.TextField{Name: "reason", Max: 255})
	c.Fields.Add(&core.BoolField{Name: "enabled"})
	dates(c)
	c.AddIndex("idx_path_policies_enabled_priority", false, "enabled, priority", "")
	return c
}
func sources() *core.Collection {
	c := base("upstream_sources")
	c.Fields.Add(&core.SelectField{Name: "key", Required: true, MaxSelect: 1, Values: []string{"default"}})
	c.Fields.Add(&core.TextField{Name: "server_id", Required: true, Max: 255})
	c.Fields.Add(&core.TextField{Name: "server_name", Max: 255})
	c.Fields.Add(&core.TextField{Name: "server_version", Max: 80})
	c.Fields.Add(&core.DateField{Name: "version_checked_at"})
	c.Fields.Add(&core.TextField{Name: "backend_username", Required: true, Max: 255})
	c.Fields.Add(&core.TextField{Name: "backend_password", Required: true})
	c.Fields.Add(&core.TextField{Name: "backend_user_id", Max: 80})
	c.Fields.Add(&core.TextField{Name: "backend_token"})
	c.Fields.Add(&core.DateField{Name: "token_updated_at"})
	c.Fields.Add(&core.DateField{Name: "last_login_at"})
	c.Fields.Add(&core.TextField{Name: "last_login_error"})
	for _, f := range []struct {
		n   string
		m   int
		req bool
	}{{"backend_user_agent", 255, true}, {"backend_authorization_client", 255, true}, {"backend_authorization_device", 255, true}, {"backend_authorization_device_id", 255, true}, {"backend_authorization_version", 80, true}} {
		c.Fields.Add(&core.TextField{Name: f.n, Max: f.m, Required: f.req})
	}
	dates(c)
	c.Fields.Add(&core.TextField{Name: "auth_generation_id", Max: 128})
	c.AddIndex("idx_upstream_sources_key", true, "key", "")
	return c
}
func endpoints(sourceID string) *core.Collection {
	c := base("upstream_endpoints")
	c.Fields.Add(rel("source", sourceID, true))
	c.Fields.Add(&core.TextField{Name: "key", Required: true, Max: 80})
	c.Fields.Add(&core.URLField{Name: "base_url", Required: true})
	c.Fields.Add(&core.BoolField{Name: "active"})
	dates(c)
	c.AddIndex("idx_upstream_endpoints_source_key", true, "source, key", "")
	c.AddIndex("idx_upstream_endpoints_source_base_url", true, "source, base_url", "")
	c.AddIndex("idx_upstream_endpoints_active_source", true, "source", "active = 1")
	return c
}
