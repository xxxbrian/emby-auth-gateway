package pbsetup

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"
	"github.com/xxxbrian/emby-auth-gateway/internal/pathpolicy"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
)

func newPoliciesCommand(app core.App) *cobra.Command {
	cmd := &cobra.Command{Use: "policies", Args: cobra.NoArgs}
	cmd.AddCommand(&cobra.Command{Use: "install-defaults", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if err := app.Bootstrap(); err != nil {
			return err
		}
		if err := pbschema.Ensure(app); err != nil {
			return err
		}
		created, preserved, err := installDefaults(app)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "default policies: created %d, preserved %d\n", created, preserved)
		return nil
	}})
	return cmd
}

func installDefaults(app core.App) (created, preserved int, err error) {
	err = app.RunInTransaction(func(tx core.App) error {
		collection, err := tx.FindCollectionByNameOrId("path_policies")
		if err != nil {
			return err
		}
		records, err := tx.FindRecordsByFilter("path_policies", "", "", 0, 0)
		if err != nil {
			return err
		}
		existing := map[string]bool{}
		for _, r := range records {
			m, p := pathpolicy.NormalizedIdentity(r.GetString("method"), r.GetString("path"))
			existing[m+"\x00"+p] = true
		}
		for _, p := range pathpolicy.Defaults() {
			m, path := pathpolicy.NormalizedIdentity(p.Method, p.Path)
			key := m + "\x00" + path
			if existing[key] {
				preserved++
				continue
			}
			r := core.NewRecord(collection)
			r.Set("method", p.Method)
			r.Set("path", p.Path)
			r.Set("action", p.Action)
			r.Set("reason", p.Reason)
			r.Set("priority", p.Priority)
			r.Set("enabled", p.Enabled)
			if err := tx.Save(r); err != nil {
				return err
			}
			existing[key] = true
			created++
		}
		return nil
	})
	return
}
