package pbmigrations

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
)

func init() {
	migrations.Register(func(app core.App) error {
		users, err := app.FindCollectionByNameOrId("gateway_users")
		if err != nil {
			return err
		}
		audit, err := app.FindCollectionByNameOrId("audit_logs")
		if err != nil {
			return err
		}
		audit.Fields.Add(&core.TextField{Name: "synthetic_user_id", Max: 80})
		audit.Fields.Add(&core.TextField{Name: "method", Max: 32})
		audit.Fields.Add(&core.TextField{Name: "path", Max: 512})
		audit.Fields.Add(&core.NumberField{Name: "status", OnlyInt: true})
		if err := app.Save(audit); err != nil {
			return err
		}

		playbackEvents := core.NewBaseCollection("playback_events")
		setCollectionRules(playbackEvents, nil, nil, nil, nil, nil)
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

		playbackStates := core.NewBaseCollection("playback_states")
		setCollectionRules(playbackStates, nil, nil, nil, nil, nil)
		playbackStates.Fields.Add(&core.RelationField{Name: "gateway_user", CollectionId: users.Id, Required: true, MaxSelect: 1})
		playbackStates.Fields.Add(&core.TextField{Name: "synthetic_user_id", Max: 80})
		playbackStates.Fields.Add(&core.TextField{Name: "item_id", Required: true, Max: 80})
		playbackStates.Fields.Add(&core.BoolField{Name: "played"})
		playbackStates.Fields.Add(&core.NumberField{Name: "playback_position_ticks", OnlyInt: true})
		playbackStates.Fields.Add(&core.NumberField{Name: "played_percentage"})
		playbackStates.Fields.Add(&core.DateField{Name: "last_played_date"})
		playbackStates.Fields.Add(&core.NumberField{Name: "play_count", OnlyInt: true})
		playbackStates.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
		playbackStates.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
		playbackStates.AddIndex("idx_playback_states_gateway_item", true, "gateway_user, item_id", "")
		if err := app.Save(playbackStates); err != nil {
			return err
		}

		pathPolicies := core.NewBaseCollection("path_policies")
		setCollectionRules(pathPolicies, nil, nil, nil, nil, nil)
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
	}, func(app core.App) error {
		for _, name := range []string{"path_policies", "playback_states", "playback_events"} {
			collection, err := app.FindCollectionByNameOrId(name)
			if err == nil {
				if err := app.Delete(collection); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
