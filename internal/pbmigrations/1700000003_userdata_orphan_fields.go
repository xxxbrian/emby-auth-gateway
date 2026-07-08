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
		if collection.Fields.GetByName("orphaned_at") == nil {
			collection.Fields.Add(&core.DateField{Name: "orphaned_at"})
		}
		if collection.Fields.GetByName("last_seen_at") == nil {
			collection.Fields.Add(&core.DateField{Name: "last_seen_at"})
		}
		return app.Save(collection)
	}, nil)
}
