package pbmigrations

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
)

var proxyAuditFieldNames = []string{"error_kind", "direction", "bytes_transferred", "duration_ms", "upstream_status", "response_committed"}

func init() {
	migrations.Register(func(app core.App) error {
		audit, err := app.FindCollectionByNameOrId("audit_logs")
		if err != nil {
			return err
		}
		if audit.Fields.GetByName("error_kind") == nil {
			audit.Fields.Add(&core.TextField{Name: "error_kind", Max: 80})
		}
		if audit.Fields.GetByName("direction") == nil {
			audit.Fields.Add(&core.TextField{Name: "direction", Max: 32})
		}
		for _, name := range []string{"bytes_transferred", "duration_ms", "upstream_status"} {
			if audit.Fields.GetByName(name) == nil {
				audit.Fields.Add(&core.NumberField{Name: name, OnlyInt: true})
			}
		}
		if audit.Fields.GetByName("response_committed") == nil {
			audit.Fields.Add(&core.BoolField{Name: "response_committed"})
		}
		return app.Save(audit)
	}, func(app core.App) error {
		audit, err := app.FindCollectionByNameOrId("audit_logs")
		if err != nil {
			return err
		}
		for _, name := range proxyAuditFieldNames {
			audit.Fields.RemoveByName(name)
		}
		return app.Save(audit)
	})
}
