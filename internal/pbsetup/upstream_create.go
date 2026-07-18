package pbsetup

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/controlplane"
	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"

	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"
)

const (
	defaultUpstreamKey = controlplane.DefaultUpstreamKey
	primaryEndpointKey = controlplane.PrimaryEndpointKey
	upstreamSources    = controlplane.UpstreamSources
	upstreamEndpoints  = controlplane.UpstreamEndpoints
)

type upstreamOptions struct {
	EmbyBaseURL                 string
	BackendUsername             string
	BackendPassword             string
	BackendUserAgent            string
	BackendAuthorizationClient  string
	BackendAuthorizationDevice  string
	BackendAuthorizationVersion string
}

type tokenOwnership = controlplane.TokenOwnership

const (
	tokenOwnershipUnknown    = controlplane.TokenOwnershipUnknown
	tokenOwnershipProtected  = controlplane.TokenOwnershipProtected
	tokenOwnershipInvocation = controlplane.TokenOwnershipInvocation
)

type upstreamState = controlplane.UpstreamState

func classifyTokenOwnership(app core.App, token string) (tokenOwnership, error) {
	return controlplane.ClassifyTokenOwnership(app, token)
}

func isTokenOwnershipError(err error) bool {
	return controlplane.IsTokenOwnershipError(err)
}

func newUpstreamCommand(app core.App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upstream",
		Short: "Prepare singleton upstream configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newUpstreamCreateCommand(app))
	return cmd
}

func newUpstreamCreateCommand(app core.App) *cobra.Command {
	var opts upstreamOptions
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create or update singleton upstream configuration",
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
			if err := runUpstreamCreate(operationCtx, app, opts); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "configured singleton upstream")
			return nil
		},
	}
	defaults := gateway.DefaultBackendClientIdentity()
	cmd.Flags().StringVar(&opts.EmbyBaseURL, "emby-url", "", "Real Emby base URL")
	cmd.Flags().StringVar(&opts.BackendUsername, "backend-username", "", "Controlled real Emby username")
	cmd.Flags().StringVar(&opts.BackendPassword, "backend-password", "", "Controlled real Emby password")
	cmd.Flags().StringVar(&opts.BackendUserAgent, "backend-user-agent", defaults.UserAgent, "User-Agent sent to the backend Emby server")
	cmd.Flags().StringVar(&opts.BackendAuthorizationClient, "backend-authorization-client", defaults.Client, "Client value sent in X-Emby-Authorization")
	cmd.Flags().StringVar(&opts.BackendAuthorizationDevice, "backend-authorization-device", defaults.Device, "Device value sent in X-Emby-Authorization")
	cmd.Flags().StringVar(&opts.BackendAuthorizationVersion, "backend-authorization-version", defaults.Version, "Version value sent in X-Emby-Authorization")
	_ = cmd.MarkFlagRequired("emby-url")
	_ = cmd.MarkFlagRequired("backend-username")
	_ = cmd.MarkFlagRequired("backend-password")
	return cmd
}

func (o upstreamOptions) validate() error {
	for name, value := range map[string]string{"--emby-url": o.EmbyBaseURL, "--backend-username": o.BackendUsername, "--backend-password": o.BackendPassword} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	return nil
}

func (o upstreamOptions) identity() gateway.BackendClientIdentity {
	return gateway.BackendClientIdentity{UserAgent: o.BackendUserAgent, Client: o.BackendAuthorizationClient, Device: o.BackendAuthorizationDevice, Version: o.BackendAuthorizationVersion}.WithDefaults()
}

func (o upstreamOptions) toInput() controlplane.UpstreamReconfigureInput {
	return controlplane.UpstreamReconfigureInput{
		EmbyBaseURL:                 o.EmbyBaseURL,
		BackendUsername:             o.BackendUsername,
		BackendPassword:             o.BackendPassword,
		BackendUserAgent:            o.BackendUserAgent,
		BackendAuthorizationClient:  o.BackendAuthorizationClient,
		BackendAuthorizationDevice:  o.BackendAuthorizationDevice,
		BackendAuthorizationVersion: o.BackendAuthorizationVersion,
		AllowCreate:                 true,
	}
}

func loadUpstreamState(app core.App) (upstreamState, error) {
	return controlplane.LoadUpstreamState(app)
}

func upstreamFingerprint(state upstreamState) string {
	return controlplane.UpstreamFingerprint(state)
}

func normalizeUpstreamURL(raw string) (string, error) {
	return controlplane.NormalizeUpstreamURL(raw)
}

func upstreamRequest(ctx context.Context, client *http.Client, method, endpoint string, body []byte, identity gateway.BackendClientIdentity, deviceID, userID, token string, output any, allowEmptySuccess bool) error {
	return controlplane.UpstreamRequest(ctx, client, method, endpoint, body, identity, deviceID, userID, token, output, allowEmptySuccess)
}

func runUpstreamCreate(parent context.Context, app core.App, opts upstreamOptions) error {
	_, err := controlplane.ReconfigureUpstream(parent, app, opts.toInput())
	return err
}
