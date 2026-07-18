package pbsetup

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"
	"github.com/xxxbrian/emby-auth-gateway/internal/controlplane"
)

func newPoliciesCommand(app core.App) *cobra.Command {
	cmd := &cobra.Command{Use: "policies", Args: cobra.NoArgs}
	cmd.AddCommand(&cobra.Command{Use: "install-defaults", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if err := app.Bootstrap(); err != nil {
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
	return controlplane.InstallDefaultPolicies(app)
}
