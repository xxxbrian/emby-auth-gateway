package pbmigrations

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
)

func init() {
	migrations.Register(func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("user_item_data")
		if err != nil {
			return err
		}
		if collection.Fields.GetByName("season_id") == nil {
			collection.Fields.Add(&core.TextField{Name: "season_id", Max: 80})
		}
		if collection.Fields.GetByName("run_time_ticks") == nil {
			collection.Fields.Add(&core.NumberField{Name: "run_time_ticks", OnlyInt: true})
		}
		collection.AddIndex("idx_user_item_data_gateway_season", false, "gateway_user, season_id", "")
		return app.Save(collection)
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("user_item_data")
		if err != nil {
			return err
		}
		collection.Fields.RemoveByName("season_id")
		collection.Fields.RemoveByName("run_time_ticks")
		collection.RemoveIndex("idx_user_item_data_gateway_season")
		return app.Save(collection)
	})
}
