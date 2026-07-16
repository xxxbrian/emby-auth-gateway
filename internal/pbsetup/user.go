package pbsetup

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"
)

type userOptions struct {
	GatewayUsername string
	GatewayPassword string
	SyntheticUserID string
}

func internalEmail(username string) string {
	replacer := strings.NewReplacer("@", "_at_", " ", "_", "/", "_", "\\", "_")
	return strings.ToLower(replacer.Replace(username)) + "@gateway.local"
}

func newUserCommand(app core.App) *cobra.Command {
	var opts userOptions
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Create or update a gateway user",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			operationCtx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			if err := opts.validate(); err != nil {
				return err
			}
			if err := app.Bootstrap(); err != nil {
				return err
			}
			if err := runUser(operationCtx, app, opts); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "configured gateway user %q\n", opts.GatewayUsername)
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.GatewayUsername, "gateway-username", "", "Gateway username exposed to Emby clients")
	cmd.Flags().StringVar(&opts.GatewayPassword, "gateway-password", "", "Gateway user password")
	cmd.Flags().StringVar(&opts.SyntheticUserID, "synthetic-user-id", "", "Synthetic Emby user id returned to clients")
	_ = cmd.MarkFlagRequired("gateway-username")
	_ = cmd.MarkFlagRequired("gateway-password")
	_ = cmd.MarkFlagRequired("synthetic-user-id")
	return cmd
}

func (o userOptions) validate() error {
	for name, value := range map[string]string{"--gateway-username": o.GatewayUsername, "--gateway-password": o.GatewayPassword, "--synthetic-user-id": o.SyntheticUserID} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	return nil
}

func runUser(ctx context.Context, app core.App, opts userOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return app.RunInTransaction(func(txApp core.App) error {
		record, err := txApp.FindFirstRecordByData("users", "username", opts.GatewayUsername)
		if err != nil {
			collection, findErr := txApp.FindCollectionByNameOrId("users")
			if findErr != nil {
				return findErr
			}
			record = core.NewRecord(collection)
		}
		record.Set("username", opts.GatewayUsername)
		record.SetEmail(internalEmail(opts.GatewayUsername))
		if !record.ValidatePassword(opts.GatewayPassword) {
			record.SetPassword(opts.GatewayPassword)
		}
		record.SetVerified(true)
		record.Set("synthetic_user_id", opts.SyntheticUserID)
		record.Set("enabled", true)
		return txApp.Save(record)
	})
}
