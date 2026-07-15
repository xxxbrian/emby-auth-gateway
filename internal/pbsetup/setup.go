package pbsetup

import (
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"

	"github.com/pocketbase/dbx"
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

	BackendUserAgent            string
	BackendAuthorizationClient  string
	BackendAuthorizationDevice  string
	BackendAuthorizationVersion string
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
	if err := app.RunInTransaction(func(txApp core.App) error {
		server, baseURLChanged, err := upsertServer(txApp, opts)
		if err != nil {
			return err
		}
		if baseURLChanged {
			if err := invalidateBackendAccountsForServer(txApp, server.Id); err != nil {
				return err
			}
		}
		account, err := upsertBackendAccount(txApp, server.Id, opts)
		if err != nil {
			return err
		}
		user, err := upsertGatewayUser(txApp, opts)
		if err != nil {
			return err
		}
		return upsertMapping(txApp, user.Id, account.Id)
	}); err != nil {
		return err
	}
	fmt.Printf("configured gateway user %q -> backend account %q (%s)\n", opts.GatewayUsername, opts.BackendAccountName, opts.EmbyBaseURL)
	return nil
}

func upsertServer(app core.App, opts options) (*core.Record, bool, error) {
	record, err := app.FindFirstRecordByData("emby_servers", "name", opts.EmbyServerName)
	if err != nil {
		collection, findErr := app.FindCollectionByNameOrId("emby_servers")
		if findErr != nil {
			return nil, false, findErr
		}
		record = core.NewRecord(collection)
	}
	baseURL := strings.TrimRight(opts.EmbyBaseURL, "/")
	baseURLChanged := record.Id != "" && strings.TrimRight(record.GetString("base_url"), "/") != baseURL
	if baseURLChanged {
		record.Set("server_id", "")
		record.Set("server_name", "")
		record.Set("server_version", "")
		record.Set("version_checked_at", nil)
	}
	record.Set("name", opts.EmbyServerName)
	record.Set("base_url", baseURL)
	identity := opts.backendClientIdentity().WithDefaults()
	deviceID := strings.TrimSpace(record.GetString("backend_authorization_device_id"))
	if deviceID == "" {
		var err error
		deviceID, err = newBackendDeviceID()
		if err != nil {
			return nil, false, err
		}
	}
	record.Set("backend_user_agent", identity.UserAgent)
	record.Set("backend_authorization_client", identity.Client)
	record.Set("backend_authorization_device", identity.Device)
	record.Set("backend_authorization_device_id", deviceID)
	record.Set("backend_authorization_version", identity.Version)
	record.Set("enabled", true)
	if err := app.Save(record); err != nil {
		return nil, false, err
	}
	return record, baseURLChanged, nil
}

func invalidateBackendAccountsForServer(app core.App, serverID string) error {
	accounts, err := app.FindRecordsByFilter(
		"backend_accounts",
		"server = {:server}",
		"",
		0,
		0,
		dbx.Params{"server": serverID},
	)
	if err != nil {
		return err
	}
	for _, account := range accounts {
		clearBackendAccountAuthState(account)
		if err := app.Save(account); err != nil {
			return err
		}
	}
	return nil
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
	credentialsChanged := record.Id != "" && (record.GetString("server") != serverID || record.GetString("backend_username") != opts.BackendUsername || record.GetString("backend_password") != opts.BackendPassword)
	record.Set("server", serverID)
	record.Set("name", opts.BackendAccountName)
	record.Set("backend_username", opts.BackendUsername)
	record.Set("backend_password", opts.BackendPassword)
	if credentialsChanged {
		clearBackendAccountAuthState(record)
	}
	record.Set("enabled", true)
	if err := app.Save(record); err != nil {
		return nil, err
	}
	return record, nil
}

func clearBackendAccountAuthState(record *core.Record) {
	record.Set("backend_user_id", "")
	record.Set("backend_token", "")
	record.Set("token_updated_at", nil)
	record.Set("last_login_at", nil)
	record.Set("last_login_error", "")
}

func (o options) backendClientIdentity() gateway.BackendClientIdentity {
	return gateway.BackendClientIdentity{
		UserAgent: o.BackendUserAgent,
		Client:    o.BackendAuthorizationClient,
		Device:    o.BackendAuthorizationDevice,
		Version:   o.BackendAuthorizationVersion,
	}
}

func newBackendDeviceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08X-%04X-%04X-%04X-%012X",
		uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3]),
		uint16(b[4])<<8|uint16(b[5]),
		uint16(b[6])<<8|uint16(b[7]),
		uint16(b[8])<<8|uint16(b[9]),
		uint64(b[10])<<40|uint64(b[11])<<32|uint64(b[12])<<24|uint64(b[13])<<16|uint64(b[14])<<8|uint64(b[15]),
	), nil
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
