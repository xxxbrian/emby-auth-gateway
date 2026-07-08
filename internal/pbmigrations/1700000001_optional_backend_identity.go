package pbmigrations

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
)

var backendIdentityFieldNames = []string{
	"backend_user_agent",
	"backend_authorization_client",
	"backend_authorization_device",
	"backend_authorization_device_id",
	"backend_authorization_version",
}

func init() {
	migrations.Register(func(app core.App) error {
		for _, collectionName := range []string{"emby_servers", "gateway_sessions"} {
			collection, err := app.FindCollectionByNameOrId(collectionName)
			if err != nil {
				return err
			}
			for _, fieldName := range backendIdentityFieldNames {
				field, ok := collection.Fields.GetByName(fieldName).(*core.TextField)
				if !ok {
					continue
				}
				field.Required = false
			}
			if err := app.Save(collection); err != nil {
				return err
			}
		}
		return nil
	}, func(app core.App) error {
		for _, collectionName := range []string{"emby_servers", "gateway_sessions"} {
			collection, err := app.FindCollectionByNameOrId(collectionName)
			if err != nil {
				return err
			}
			for _, fieldName := range backendIdentityFieldNames {
				field, ok := collection.Fields.GetByName(fieldName).(*core.TextField)
				if !ok {
					continue
				}
				field.Required = true
			}
			if err := app.Save(collection); err != nil {
				return err
			}
		}
		return nil
	})
}
