package main

import (
	"fmt"

	"github.com/xxxbrian/emby-auth-gateway/internal/version"

	"github.com/spf13/cobra"
)

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print gateway build version metadata",
		Run: func(cmd *cobra.Command, args []string) {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "version: %s\n", version.Version)
			fmt.Fprintf(out, "commit: %s\n", version.Commit)
			fmt.Fprintf(out, "date: %s\n", version.Date)
		},
	}
}
