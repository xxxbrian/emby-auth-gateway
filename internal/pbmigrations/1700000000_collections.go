package pbmigrations

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
)

func init() {
	migrations.Register(func(app core.App) error {
		users, err := app.FindCollectionByNameOrId("users")
		if err != nil {
			return err
		}
		lockCollection(users)
		users.PasswordAuth.Enabled = false
		users.PasswordAuth.IdentityFields = []string{"username"}
		users.Fields.Add(&core.TextField{Name: "username", Required: true, Max: 255})
		users.Fields.Add(&core.TextField{Name: "synthetic_user_id", Required: true, Max: 80})
		users.Fields.Add(&core.BoolField{Name: "enabled"})
		users.AddIndex("idx_users_username", true, "username", "")
		users.AddIndex("idx_users_synthetic", true, "synthetic_user_id", "")
		if err := app.Save(users); err != nil {
			return err
		}

		servers := core.NewBaseCollection("emby_servers")
		lockCollection(servers)
		servers.Fields.Add(&core.TextField{Name: "name", Required: true, Max: 255})
		servers.Fields.Add(&core.URLField{Name: "base_url", Required: true})
		servers.Fields.Add(&core.BoolField{Name: "enabled"})
		servers.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
		servers.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
		if err := app.Save(servers); err != nil {
			return err
		}

		accounts := core.NewBaseCollection("backend_accounts")
		lockCollection(accounts)
		accounts.Fields.Add(&core.RelationField{Name: "server", CollectionId: servers.Id, Required: true, MaxSelect: 1})
		accounts.Fields.Add(&core.TextField{Name: "name", Required: true, Max: 255})
		accounts.Fields.Add(&core.TextField{Name: "backend_username", Required: true, Max: 255})
		accounts.Fields.Add(&core.TextField{Name: "backend_password_encrypted", Required: true})
		accounts.Fields.Add(&core.BoolField{Name: "enabled"})
		accounts.Fields.Add(&core.DateField{Name: "last_login_at"})
		accounts.Fields.Add(&core.TextField{Name: "last_login_error"})
		accounts.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
		accounts.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
		if err := app.Save(accounts); err != nil {
			return err
		}

		mappings := core.NewBaseCollection("user_mappings")
		lockCollection(mappings)
		mappings.Fields.Add(&core.RelationField{Name: "gateway_user", CollectionId: users.Id, Required: true, MaxSelect: 1})
		mappings.Fields.Add(&core.RelationField{Name: "backend_account", CollectionId: accounts.Id, Required: true, MaxSelect: 1})
		mappings.Fields.Add(&core.BoolField{Name: "enabled"})
		mappings.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
		mappings.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
		mappings.AddIndex("idx_user_mappings_gateway_user", true, "gateway_user", "")
		if err := app.Save(mappings); err != nil {
			return err
		}

		sessions := core.NewBaseCollection("gateway_sessions")
		lockCollection(sessions)
		sessions.Fields.Add(&core.TextField{Name: "gateway_token_hash", Required: true, Max: 128})
		sessions.Fields.Add(&core.RelationField{Name: "gateway_user", CollectionId: users.Id, Required: true, MaxSelect: 1})
		sessions.Fields.Add(&core.TextField{Name: "gateway_username", Max: 255})
		sessions.Fields.Add(&core.TextField{Name: "synthetic_user_id", Required: true, Max: 80})
		sessions.Fields.Add(&core.RelationField{Name: "backend_account", CollectionId: accounts.Id, Required: true, MaxSelect: 1})
		sessions.Fields.Add(&core.TextField{Name: "backend_server_id", Max: 255})
		sessions.Fields.Add(&core.URLField{Name: "backend_base_url", Required: true})
		sessions.Fields.Add(&core.TextField{Name: "backend_user_id", Required: true, Max: 80})
		sessions.Fields.Add(&core.TextField{Name: "backend_username", Max: 255})
		sessions.Fields.Add(&core.TextField{Name: "backend_token_encrypted", Required: true})
		sessions.Fields.Add(&core.TextField{Name: "client", Max: 255})
		sessions.Fields.Add(&core.TextField{Name: "device", Max: 255})
		sessions.Fields.Add(&core.TextField{Name: "device_id", Max: 255})
		sessions.Fields.Add(&core.TextField{Name: "version", Max: 80})
		sessions.Fields.Add(&core.TextField{Name: "remote_ip", Max: 80})
		sessions.Fields.Add(&core.DateField{Name: "expires_at", Required: true})
		sessions.Fields.Add(&core.DateField{Name: "revoked_at"})
		sessions.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
		sessions.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
		sessions.AddIndex("idx_gateway_sessions_token_hash", true, "gateway_token_hash", "")
		if err := app.Save(sessions); err != nil {
			return err
		}

		audit := core.NewBaseCollection("audit_logs")
		lockCollection(audit)
		audit.Fields.Add(&core.RelationField{Name: "gateway_user", CollectionId: users.Id, MaxSelect: 1})
		audit.Fields.Add(&core.TextField{Name: "synthetic_user_id", Max: 80})
		audit.Fields.Add(&core.TextField{Name: "event", Required: true, Max: 255})
		audit.Fields.Add(&core.TextField{Name: "message"})
		audit.Fields.Add(&core.TextField{Name: "method", Max: 32})
		audit.Fields.Add(&core.TextField{Name: "path", Max: 512})
		audit.Fields.Add(&core.NumberField{Name: "status", OnlyInt: true})
		audit.Fields.Add(&core.TextField{Name: "remote_ip", Max: 80})
		audit.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
		if err := app.Save(audit); err != nil {
			return err
		}

		playbackEvents := core.NewBaseCollection("playback_events")
		lockCollection(playbackEvents)
		playbackEvents.Fields.Add(&core.RelationField{Name: "gateway_user", CollectionId: users.Id, Required: true, MaxSelect: 1})
		playbackEvents.Fields.Add(&core.TextField{Name: "synthetic_user_id", Max: 80})
		playbackEvents.Fields.Add(&core.TextField{Name: "item_id", Required: true, Max: 80})
		playbackEvents.Fields.Add(&core.TextField{Name: "item_name", Max: 255})
		playbackEvents.Fields.Add(&core.SelectField{Name: "event", Required: true, Values: []string{"playing", "progress", "stopped"}})
		playbackEvents.Fields.Add(&core.NumberField{Name: "playback_position_ticks", OnlyInt: true})
		playbackEvents.Fields.Add(&core.BoolField{Name: "played"})
		playbackEvents.Fields.Add(&core.NumberField{Name: "played_percentage"})
		playbackEvents.Fields.Add(&core.TextField{Name: "remote_ip", Max: 80})
		playbackEvents.Fields.Add(&core.DateField{Name: "occurred_at", Required: true})
		playbackEvents.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
		playbackEvents.AddIndex("idx_playback_events_gateway_item", false, "gateway_user, item_id", "")
		if err := app.Save(playbackEvents); err != nil {
			return err
		}

		userItemData := core.NewBaseCollection("user_item_data")
		lockCollection(userItemData)
		userItemData.Fields.Add(&core.RelationField{Name: "gateway_user", CollectionId: users.Id, Required: true, MaxSelect: 1})
		userItemData.Fields.Add(&core.TextField{Name: "synthetic_user_id", Max: 80})
		userItemData.Fields.Add(&core.TextField{Name: "item_id", Required: true, Max: 80})
		userItemData.Fields.Add(&core.TextField{Name: "item_name", Max: 255})
		userItemData.Fields.Add(&core.TextField{Name: "item_type", Max: 80})
		userItemData.Fields.Add(&core.TextField{Name: "series_id", Max: 80})
		userItemData.Fields.Add(&core.TextField{Name: "series_name", Max: 255})
		userItemData.Fields.Add(&core.NumberField{Name: "index_number", OnlyInt: true})
		userItemData.Fields.Add(&core.NumberField{Name: "parent_index_number", OnlyInt: true})
		userItemData.Fields.Add(&core.BoolField{Name: "played"})
		userItemData.Fields.Add(&core.NumberField{Name: "playback_position_ticks", OnlyInt: true})
		userItemData.Fields.Add(&core.NumberField{Name: "played_percentage"})
		userItemData.Fields.Add(&core.BoolField{Name: "played_percentage_set"})
		userItemData.Fields.Add(&core.DateField{Name: "last_played_date"})
		userItemData.Fields.Add(&core.NumberField{Name: "play_count", OnlyInt: true})
		userItemData.Fields.Add(&core.BoolField{Name: "is_favorite"})
		userItemData.Fields.Add(&core.BoolField{Name: "likes"})
		userItemData.Fields.Add(&core.BoolField{Name: "likes_set"})
		userItemData.Fields.Add(&core.TextField{Name: "fingerprint", Max: 255})
		userItemData.Fields.Add(&core.DateField{Name: "orphaned_at"})
		userItemData.Fields.Add(&core.DateField{Name: "last_seen_at"})
		userItemData.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
		userItemData.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
		userItemData.AddIndex("idx_user_item_data_gateway_item", true, "gateway_user, item_id", "")
		userItemData.AddIndex("idx_user_item_data_gateway_series", false, "gateway_user, series_id", "")
		if err := app.Save(userItemData); err != nil {
			return err
		}

		displayPreferences := core.NewBaseCollection("display_preferences")
		lockCollection(displayPreferences)
		displayPreferences.Fields.Add(&core.RelationField{Name: "gateway_user", CollectionId: users.Id, Required: true, MaxSelect: 1})
		displayPreferences.Fields.Add(&core.TextField{Name: "synthetic_user_id", Max: 80})
		displayPreferences.Fields.Add(&core.TextField{Name: "preference_id", Required: true, Max: 255})
		displayPreferences.Fields.Add(&core.TextField{Name: "client", Max: 255})
		displayPreferences.Fields.Add(&core.TextField{Name: "payload_json", Required: true, Max: 1048576})
		displayPreferences.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
		displayPreferences.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
		displayPreferences.AddIndex("idx_display_preferences_gateway_pref_client", true, "gateway_user, preference_id, client", "")
		if err := app.Save(displayPreferences); err != nil {
			return err
		}

		pathPolicies := core.NewBaseCollection("path_policies")
		lockCollection(pathPolicies)
		pathPolicies.Fields.Add(&core.TextField{Name: "method", Required: true, Max: 32})
		pathPolicies.Fields.Add(&core.TextField{Name: "path", Required: true, Max: 512})
		pathPolicies.Fields.Add(&core.SelectField{Name: "action", Required: true, Values: []string{"allow", "deny"}})
		pathPolicies.Fields.Add(&core.NumberField{Name: "priority", OnlyInt: true})
		pathPolicies.Fields.Add(&core.TextField{Name: "reason", Max: 255})
		pathPolicies.Fields.Add(&core.BoolField{Name: "enabled"})
		pathPolicies.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
		pathPolicies.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
		pathPolicies.AddIndex("idx_path_policies_enabled_priority", false, "enabled, priority", "")
		return app.Save(pathPolicies)
	}, nil)
}

func gatewayCollectionNames() []string {
	return []string{"users", "emby_servers", "backend_accounts", "user_mappings", "gateway_sessions", "audit_logs", "playback_events", "user_item_data", "display_preferences", "path_policies"}
}

func lockCollection(collection *core.Collection) {
	collection.ListRule = nil
	collection.ViewRule = nil
	collection.CreateRule = nil
	collection.UpdateRule = nil
	collection.DeleteRule = nil
}
