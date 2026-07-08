package pbmigrations

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
	"github.com/pocketbase/pocketbase/tools/types"
)

func init() {
	migrations.Register(func(app core.App) error {
		for _, name := range gatewayCollectionNames() {
			collection, err := app.FindCollectionByNameOrId(name)
			if err != nil {
				return err
			}
			setCollectionRules(collection, nil, nil, nil, nil, nil)
			if collection.Name == "gateway_users" {
				collection.PasswordAuth.Enabled = false
			}
			if err := app.Save(collection); err != nil {
				return err
			}
		}
		return nil
	}, func(app core.App) error {
		for _, name := range gatewayCollectionNames() {
			collection, err := app.FindCollectionByNameOrId(name)
			if err != nil {
				return err
			}
			switch collection.Name {
			case "gateway_users":
				setCollectionRules(collection,
					types.Pointer("@request.auth.collectionName = 'gateway_users'"),
					types.Pointer("id = @request.auth.id"),
					types.Pointer(""),
					types.Pointer("id = @request.auth.id"),
					types.Pointer("id = @request.auth.id"),
				)
				collection.PasswordAuth.Enabled = true
				collection.PasswordAuth.IdentityFields = []string{"username"}
			default:
				empty := types.Pointer("")
				setCollectionRules(collection, empty, empty, empty, empty, empty)
			}
			if err := app.Save(collection); err != nil {
				return err
			}
		}
		return nil
	})
}

func gatewayCollectionNames() []string {
	return []string{"gateway_users", "emby_servers", "backend_accounts", "user_mappings", "gateway_sessions", "audit_logs"}
}

func setCollectionRules(collection *core.Collection, list, view, create, update, delete *string) {
	collection.ListRule = list
	collection.ViewRule = view
	collection.CreateRule = create
	collection.UpdateRule = update
	collection.DeleteRule = delete
}
