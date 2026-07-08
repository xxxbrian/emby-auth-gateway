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

		userItemData := core.NewBaseCollection("user_item_data")
		setCollectionRules(userItemData, nil, nil, nil, nil, nil)
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
		setCollectionRules(displayPreferences, nil, nil, nil, nil, nil)
		displayPreferences.Fields.Add(&core.RelationField{Name: "gateway_user", CollectionId: users.Id, Required: true, MaxSelect: 1})
		displayPreferences.Fields.Add(&core.TextField{Name: "synthetic_user_id", Max: 80})
		displayPreferences.Fields.Add(&core.TextField{Name: "preference_id", Required: true, Max: 255})
		displayPreferences.Fields.Add(&core.TextField{Name: "client", Max: 255})
		displayPreferences.Fields.Add(&core.TextField{Name: "payload_json", Required: true, Max: 1048576})
		displayPreferences.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
		displayPreferences.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
		displayPreferences.AddIndex("idx_display_preferences_gateway_pref_client", true, "gateway_user, preference_id, client", "")
		return app.Save(displayPreferences)
	}, func(app core.App) error {
		for _, name := range []string{"display_preferences", "user_item_data"} {
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
