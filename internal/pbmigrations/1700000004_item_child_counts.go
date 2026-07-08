package pbmigrations

import (
	"strings"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
)

func init() {
	migrations.Register(func(app core.App) error {
		if _, err := app.FindCollectionByNameOrId("item_child_counts"); err == nil {
			return nil
		}
		collection := core.NewBaseCollection("item_child_counts")
		lockCollection(collection)
		collection.Fields.Add(&core.TextField{Name: "backend_account_id", Required: true, Max: 80})
		collection.Fields.Add(&core.TextField{Name: "item_id", Required: true, Max: 80})
		collection.Fields.Add(&core.NumberField{Name: "child_count", Required: true, OnlyInt: true})
		collection.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
		collection.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
		collection.AddIndex("idx_item_child_counts_account_item", true, "backend_account_id, item_id", "")
		return app.Save(collection)
	}, func(app core.App) error {
		collection, err := app.FindCollectionByNameOrId("item_child_counts")
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				return nil
			}
			return err
		}
		return app.Delete(collection)
	})
}
