package pbmigrations

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
)

var gatewaySessionBackendSnapshotFields = []string{
	"backend_server_id",
	"backend_base_url",
	"backend_user_id",
	"backend_username",
	"backend_token",
	"backend_user_agent",
	"backend_authorization_client",
	"backend_authorization_device",
	"backend_authorization_device_id",
	"backend_authorization_version",
}

func init() {
	migrations.Register(func(app core.App) error {
		servers, err := app.FindCollectionByNameOrId("emby_servers")
		if err != nil {
			return err
		}
		if servers.Fields.GetByName("server_id") == nil {
			servers.Fields.Add(&core.TextField{Name: "server_id", Max: 255})
		}
		if servers.Fields.GetByName("server_name") == nil {
			servers.Fields.Add(&core.TextField{Name: "server_name", Max: 255})
		}
		if servers.Fields.GetByName("server_version") == nil {
			servers.Fields.Add(&core.TextField{Name: "server_version", Max: 80})
		}
		if servers.Fields.GetByName("version_checked_at") == nil {
			servers.Fields.Add(&core.DateField{Name: "version_checked_at"})
		}
		if err := app.Save(servers); err != nil {
			return err
		}

		accounts, err := app.FindCollectionByNameOrId("backend_accounts")
		if err != nil {
			return err
		}
		if accounts.Fields.GetByName("backend_user_id") == nil {
			accounts.Fields.Add(&core.TextField{Name: "backend_user_id", Max: 80})
		}
		if accounts.Fields.GetByName("backend_token") == nil {
			accounts.Fields.Add(&core.TextField{Name: "backend_token"})
		}
		if accounts.Fields.GetByName("token_updated_at") == nil {
			accounts.Fields.Add(&core.DateField{Name: "token_updated_at"})
		}
		if err := app.Save(accounts); err != nil {
			return err
		}

		sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
		if err != nil {
			return err
		}
		for _, name := range gatewaySessionBackendSnapshotFields {
			sessions.Fields.RemoveByName(name)
		}
		return app.Save(sessions)
	}, func(app core.App) error {
		sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
		if err != nil {
			return err
		}
		if sessions.Fields.GetByName("backend_server_id") == nil {
			sessions.Fields.Add(&core.TextField{Name: "backend_server_id", Max: 255})
		}
		if sessions.Fields.GetByName("backend_base_url") == nil {
			sessions.Fields.Add(&core.URLField{Name: "backend_base_url"})
		}
		if sessions.Fields.GetByName("backend_user_id") == nil {
			sessions.Fields.Add(&core.TextField{Name: "backend_user_id", Max: 80})
		}
		if sessions.Fields.GetByName("backend_username") == nil {
			sessions.Fields.Add(&core.TextField{Name: "backend_username", Max: 255})
		}
		if sessions.Fields.GetByName("backend_token") == nil {
			sessions.Fields.Add(&core.TextField{Name: "backend_token"})
		}
		for _, name := range backendIdentityFieldNames {
			if sessions.Fields.GetByName(name) == nil {
				sessions.Fields.Add(&core.TextField{Name: name, Max: 255})
			}
		}
		if err := app.Save(sessions); err != nil {
			return err
		}

		accounts, err := app.FindCollectionByNameOrId("backend_accounts")
		if err != nil {
			return err
		}
		accounts.Fields.RemoveByName("backend_user_id")
		accounts.Fields.RemoveByName("backend_token")
		accounts.Fields.RemoveByName("token_updated_at")
		if err := app.Save(accounts); err != nil {
			return err
		}

		servers, err := app.FindCollectionByNameOrId("emby_servers")
		if err != nil {
			return err
		}
		servers.Fields.RemoveByName("server_id")
		servers.Fields.RemoveByName("server_name")
		servers.Fields.RemoveByName("server_version")
		servers.Fields.RemoveByName("version_checked_at")
		return app.Save(servers)
	})
}
