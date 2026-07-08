package pbsetup

import (
	"fmt"
	"strings"

	"emby-auth-gateway/internal/gateway"

	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"
)

func NewCommand(app core.App) *cobra.Command {
	var opts options
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Creates or updates gateway user, backend account, and mapping records",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := opts.validate(); err != nil {
				return err
			}
			if err := app.Bootstrap(); err != nil {
				return err
			}
			return run(app, opts)
		},
	}
	defaults := gateway.DefaultBackendClientIdentity()

	cmd.Flags().StringVar(&opts.GatewayUsername, "gateway-username", "", "Gateway username exposed to Emby clients")
	cmd.Flags().StringVar(&opts.GatewayPassword, "gateway-password", "", "Gateway user password")
	cmd.Flags().StringVar(&opts.SyntheticUserID, "synthetic-user-id", "", "Synthetic Emby user id returned to clients")
	cmd.Flags().StringVar(&opts.EmbyServerName, "emby-server-name", "default", "Internal Emby server display name")
	cmd.Flags().StringVar(&opts.EmbyBaseURL, "emby-url", "", "Real Emby base URL, for example http://10.0.0.5:8096/emby")
	cmd.Flags().StringVar(&opts.BackendAccountName, "backend-account-name", "default", "Backend account display name")
	cmd.Flags().StringVar(&opts.BackendUsername, "backend-username", "", "Controlled real Emby username")
	cmd.Flags().StringVar(&opts.BackendPassword, "backend-password", "", "Controlled real Emby password")
	cmd.Flags().StringVar(&opts.BackendUserAgent, "backend-user-agent", defaults.UserAgent, "User-Agent sent to the backend Emby server")
	cmd.Flags().StringVar(&opts.BackendAuthorizationClient, "backend-authorization-client", defaults.Client, "Client value sent in X-Emby-Authorization to the backend")
	cmd.Flags().StringVar(&opts.BackendAuthorizationDevice, "backend-authorization-device", defaults.Device, "Device value sent in X-Emby-Authorization to the backend")
	cmd.Flags().StringVar(&opts.BackendAuthorizationDeviceID, "backend-authorization-device-id", defaults.DeviceID, "DeviceId value sent in X-Emby-Authorization to the backend")
	cmd.Flags().StringVar(&opts.BackendAuthorizationVersion, "backend-authorization-version", defaults.Version, "Version value sent in X-Emby-Authorization to the backend")
	return cmd
}

type options struct {
	GatewayUsername    string
	GatewayPassword    string
	SyntheticUserID    string
	EmbyServerName     string
	EmbyBaseURL        string
	BackendAccountName string
	BackendUsername    string
	BackendPassword    string

	BackendUserAgent             string
	BackendAuthorizationClient   string
	BackendAuthorizationDevice   string
	BackendAuthorizationDeviceID string
	BackendAuthorizationVersion  string
}

func (o options) validate() error {
	required := map[string]string{
		"--gateway-username":  o.GatewayUsername,
		"--gateway-password":  o.GatewayPassword,
		"--synthetic-user-id": o.SyntheticUserID,
		"--emby-url":          o.EmbyBaseURL,
		"--backend-username":  o.BackendUsername,
		"--backend-password":  o.BackendPassword,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	return nil
}

func run(app core.App, opts options) error {
	server, err := upsertServer(app, opts)
	if err != nil {
		return err
	}
	account, err := upsertBackendAccount(app, server.Id, opts)
	if err != nil {
		return err
	}
	user, err := upsertGatewayUser(app, opts)
	if err != nil {
		return err
	}
	if err := upsertMapping(app, user.Id, account.Id); err != nil {
		return err
	}
	fmt.Printf("configured gateway user %q -> backend account %q (%s)\n", opts.GatewayUsername, opts.BackendAccountName, opts.EmbyBaseURL)
	return nil
}

func upsertServer(app core.App, opts options) (*core.Record, error) {
	record, err := app.FindFirstRecordByData("emby_servers", "name", opts.EmbyServerName)
	if err != nil {
		collection, findErr := app.FindCollectionByNameOrId("emby_servers")
		if findErr != nil {
			return nil, findErr
		}
		record = core.NewRecord(collection)
	}
	record.Set("name", opts.EmbyServerName)
	record.Set("base_url", strings.TrimRight(opts.EmbyBaseURL, "/"))
	identity := opts.backendClientIdentity().WithDefaults()
	record.Set("backend_user_agent", identity.UserAgent)
	record.Set("backend_authorization_client", identity.Client)
	record.Set("backend_authorization_device", identity.Device)
	record.Set("backend_authorization_device_id", identity.DeviceID)
	record.Set("backend_authorization_version", identity.Version)
	record.Set("enabled", true)
	if err := app.Save(record); err != nil {
		return nil, err
	}
	return record, nil
}

func upsertBackendAccount(app core.App, serverID string, opts options) (*core.Record, error) {
	record, err := app.FindFirstRecordByData("backend_accounts", "name", opts.BackendAccountName)
	if err != nil {
		collection, findErr := app.FindCollectionByNameOrId("backend_accounts")
		if findErr != nil {
			return nil, findErr
		}
		record = core.NewRecord(collection)
	}
	record.Set("server", serverID)
	record.Set("name", opts.BackendAccountName)
	record.Set("backend_username", opts.BackendUsername)
	record.Set("backend_password", opts.BackendPassword)
	record.Set("enabled", true)
	if err := app.Save(record); err != nil {
		return nil, err
	}
	return record, nil
}

func (o options) backendClientIdentity() gateway.BackendClientIdentity {
	return gateway.BackendClientIdentity{
		UserAgent: o.BackendUserAgent,
		Client:    o.BackendAuthorizationClient,
		Device:    o.BackendAuthorizationDevice,
		DeviceID:  o.BackendAuthorizationDeviceID,
		Version:   o.BackendAuthorizationVersion,
	}
}

func upsertGatewayUser(app core.App, opts options) (*core.Record, error) {
	record, err := app.FindFirstRecordByData("users", "username", opts.GatewayUsername)
	if err != nil {
		collection, findErr := app.FindCollectionByNameOrId("users")
		if findErr != nil {
			return nil, findErr
		}
		record = core.NewRecord(collection)
	}
	record.Set("username", opts.GatewayUsername)
	record.SetEmail(internalEmail(opts.GatewayUsername))
	record.SetPassword(opts.GatewayPassword)
	record.SetVerified(true)
	record.Set("synthetic_user_id", opts.SyntheticUserID)
	record.Set("enabled", true)
	if err := app.Save(record); err != nil {
		return nil, err
	}
	return record, nil
}

func internalEmail(username string) string {
	replacer := strings.NewReplacer("@", "_at_", " ", "_", "/", "_", "\\", "_")
	return strings.ToLower(replacer.Replace(username)) + "@gateway.local"
}

func upsertMapping(app core.App, gatewayUserID, backendAccountID string) error {
	record, err := app.FindFirstRecordByData("user_mappings", "gateway_user", gatewayUserID)
	if err != nil {
		collection, findErr := app.FindCollectionByNameOrId("user_mappings")
		if findErr != nil {
			return findErr
		}
		record = core.NewRecord(collection)
	}
	record.Set("gateway_user", gatewayUserID)
	record.Set("backend_account", backendAccountID)
	record.Set("enabled", true)
	return app.Save(record)
}
