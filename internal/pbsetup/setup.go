package pbsetup

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"
)

func NewCommand(app core.App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Configure gateway upstreams and users",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.FParseErrWhitelist.UnknownFlags = false
	cmd.AddCommand(newUpstreamCommand(app), newUserCommand(app))
	return cmd
}
