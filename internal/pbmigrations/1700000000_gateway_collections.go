package pbmigrations

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
	"github.com/pocketbase/pocketbase/tools/types"
)

func init() {
	migrations.Register(func(app core.App) error {
		users := core.NewAuthCollection("gateway_users")
		users.ListRule = types.Pointer("@request.auth.collectionName = 'gateway_users'")
		users.ViewRule = types.Pointer("id = @request.auth.id")
		users.CreateRule = types.Pointer("")
		users.UpdateRule = types.Pointer("id = @request.auth.id")
		users.DeleteRule = types.Pointer("id = @request.auth.id")
		users.Fields.Add(&core.TextField{Name: "synthetic_user_id", Required: true, Max: 80})
		users.Fields.Add(&core.BoolField{Name: "enabled"})
		users.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
		users.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
		users.AddIndex("idx_gateway_users_synthetic", true, "synthetic_user_id", "")
		if err := app.Save(users); err != nil {
			return err
		}

		servers := core.NewBaseCollection("emby_servers")
		servers.ListRule = types.Pointer("")
		servers.ViewRule = types.Pointer("")
		servers.CreateRule = types.Pointer("")
		servers.UpdateRule = types.Pointer("")
		servers.DeleteRule = types.Pointer("")
		servers.Fields.Add(&core.TextField{Name: "name", Required: true, Max: 255})
		servers.Fields.Add(&core.URLField{Name: "base_url", Required: true})
		servers.Fields.Add(&core.BoolField{Name: "enabled"})
		servers.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
		servers.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
		if err := app.Save(servers); err != nil {
			return err
		}

		accounts := core.NewBaseCollection("backend_accounts")
		accounts.ListRule = types.Pointer("")
		accounts.ViewRule = types.Pointer("")
		accounts.CreateRule = types.Pointer("")
		accounts.UpdateRule = types.Pointer("")
		accounts.DeleteRule = types.Pointer("")
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
		mappings.ListRule = types.Pointer("")
		mappings.ViewRule = types.Pointer("")
		mappings.CreateRule = types.Pointer("")
		mappings.UpdateRule = types.Pointer("")
		mappings.DeleteRule = types.Pointer("")
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
		sessions.ListRule = types.Pointer("")
		sessions.ViewRule = types.Pointer("")
		sessions.CreateRule = types.Pointer("")
		sessions.UpdateRule = types.Pointer("")
		sessions.DeleteRule = types.Pointer("")
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
		audit.ListRule = types.Pointer("")
		audit.ViewRule = types.Pointer("")
		audit.CreateRule = types.Pointer("")
		audit.UpdateRule = types.Pointer("")
		audit.DeleteRule = types.Pointer("")
		audit.Fields.Add(&core.RelationField{Name: "gateway_user", CollectionId: users.Id, MaxSelect: 1})
		audit.Fields.Add(&core.TextField{Name: "event", Required: true, Max: 255})
		audit.Fields.Add(&core.TextField{Name: "message"})
		audit.Fields.Add(&core.TextField{Name: "remote_ip", Max: 80})
		audit.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
		return app.Save(audit)
	}, func(app core.App) error {
		for _, name := range []string{"audit_logs", "gateway_sessions", "user_mappings", "backend_accounts", "emby_servers", "gateway_users"} {
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
