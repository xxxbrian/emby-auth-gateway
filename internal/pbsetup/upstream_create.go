package pbsetup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"
)

const (
	defaultUpstreamKey = "default"
	primaryEndpointKey = "primary"
	upstreamSources    = "upstream_sources"
	upstreamEndpoints  = "upstream_endpoints"
)

// afterUpstreamProbe is a test hook for deterministic cancellation/drift tests.
var afterUpstreamProbe func()
var afterUpstreamSourceSave func()

type upstreamOptions struct {
	EmbyBaseURL                 string
	BackendUsername             string
	BackendPassword             string
	BackendUserAgent            string
	BackendAuthorizationClient  string
	BackendAuthorizationDevice  string
	BackendAuthorizationVersion string
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
		Short: "Create or update preparatory singleton upstream configuration",
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
			fmt.Printf("configured preparatory singleton upstream; it will be used after runtime cutover\n")
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

type upstreamState struct {
	source       *core.Record
	endpoints    []*core.Record
	allEndpoints []*core.Record
	fingerprint  string
}

func loadUpstreamState(app core.App) (upstreamState, error) {
	state := upstreamState{}
	source, err := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
	if err == nil {
		state.source = source
	}
	if err != nil && !isNotFound(err) {
		return state, err
	}
	endpoints, err := app.FindRecordsByFilter(upstreamEndpoints, "", "id", 0, 0, nil)
	if err != nil {
		return state, err
	}
	state.allEndpoints = endpoints
	state.endpoints = endpoints
	if state.source == nil && len(endpoints) > 0 {
		return state, fmt.Errorf("refusing setup: orphan upstream endpoint rows exist")
	}
	if state.source != nil {
		state.endpoints, err = app.FindRecordsByFilter(upstreamEndpoints, "source = {:source}", "id", 0, 0, dbx.Params{"source": state.source.Id})
		if err != nil {
			return state, err
		}
	}
	state.fingerprint = upstreamFingerprint(state)
	return state, nil
}

func isNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || strings.Contains(strings.ToLower(err.Error()), "not found")
}

func upstreamFingerprint(state upstreamState) string {
	type sourceSnapshot struct {
		ID, Key, ServerID, ServerName, ServerVersion, VersionCheckedAt, BackendUsername, BackendPassword, BackendUserID, BackendToken, TokenUpdatedAt, LastLoginAt, LastLoginError, UserAgent, Client, Device, DeviceID, Version, Updated string
	}
	type endpointSnapshot struct {
		ID, Source, Key, BaseURL, Updated string
		Active                            bool
	}
	type snapshot struct {
		Source    *sourceSnapshot
		Endpoints []endpointSnapshot
	}
	s := snapshot{}
	if state.source != nil {
		s.Source = &sourceSnapshot{state.source.Id, state.source.GetString("key"), state.source.GetString("server_id"), state.source.GetString("server_name"), state.source.GetString("server_version"), state.source.GetDateTime("version_checked_at").String(), state.source.GetString("backend_username"), state.source.GetString("backend_password"), state.source.GetString("backend_user_id"), state.source.GetString("backend_token"), state.source.GetDateTime("token_updated_at").String(), state.source.GetDateTime("last_login_at").String(), state.source.GetString("last_login_error"), state.source.GetString("backend_user_agent"), state.source.GetString("backend_authorization_client"), state.source.GetString("backend_authorization_device"), state.source.GetString("backend_authorization_device_id"), state.source.GetString("backend_authorization_version"), state.source.GetDateTime("updated").String()}
	}
	for _, endpoint := range state.allEndpoints {
		s.Endpoints = append(s.Endpoints, endpointSnapshot{endpoint.Id, endpoint.GetString("source"), endpoint.GetString("key"), endpoint.GetString("base_url"), endpoint.GetDateTime("updated").String(), endpoint.GetBool("active")})
	}
	sort.Slice(s.Endpoints, func(i, j int) bool { return s.Endpoints[i].ID < s.Endpoints[j].ID })
	data, _ := json.Marshal(s)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func activeEndpoint(endpoints []*core.Record) (*core.Record, error) {
	var active *core.Record
	for _, endpoint := range endpoints {
		if endpoint.GetBool("active") {
			if active != nil {
				return nil, fmt.Errorf("refusing setup: source has multiple active endpoints")
			}
			active = endpoint
		}
	}
	if active == nil {
		return nil, fmt.Errorf("refusing setup: source has no active endpoint")
	}
	return active, nil
}

func runUpstreamCreate(parent context.Context, app core.App, opts upstreamOptions) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	baseURL, err := normalizeUpstreamURL(opts.EmbyBaseURL)
	if err != nil {
		return err
	}
	state, err := loadUpstreamState(app)
	if err != nil {
		return err
	}
	identity := opts.identity()
	var deviceID string
	var oldToken, oldURL, oldUserID string
	var oldIdentity gateway.BackendClientIdentity
	if state.source != nil {
		active, err := activeEndpoint(state.endpoints)
		if err != nil {
			return err
		}
		deviceID = state.source.GetString("backend_authorization_device_id")
		if deviceID == "" {
			return fmt.Errorf("refusing setup: stored source has no device ID")
		}
		oldToken, oldURL, oldUserID = state.source.GetString("backend_token"), active.GetString("base_url"), state.source.GetString("backend_user_id")
		oldIdentity = gateway.BackendClientIdentity{UserAgent: state.source.GetString("backend_user_agent"), Client: state.source.GetString("backend_authorization_client"), Device: state.source.GetString("backend_authorization_device"), Version: state.source.GetString("backend_authorization_version")}.WithDefaults()
		if err := rejectEndpointCollision(app, baseURL, state.source); err != nil {
			return err
		}
		if completeNoop(state.source, active, baseURL, opts, identity) {
			return nil
		}
	} else {
		deviceID, err = newBackendDeviceID()
		if err != nil {
			return err
		}
	}
	if state.source == nil {
		if err := rejectEndpointCollision(app, baseURL, nil); err != nil {
			return err
		}
	}
	expectedID := ""
	if state.source != nil {
		expectedID = state.source.GetString("server_id")
	}
	probe, err := probeUpstream(ctx, baseURL, opts.BackendUsername, opts.BackendPassword, deviceID, expectedID, identity)
	if err != nil {
		if probe.token != "" {
			logoutUpstream(ctx, baseURL, identity, deviceID, probe.userID, probe.token)
		}
		return err
	}
	if afterUpstreamProbe != nil {
		afterUpstreamProbe()
	}
	if err := ctx.Err(); err != nil {
		logoutUpstream(ctx, baseURL, identity, deviceID, probe.userID, probe.token)
		return err
	}
	committed := false
	err = app.RunInTransaction(func(txApp core.App) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := loadUpstreamState(txApp)
		if err != nil {
			return err
		}
		if current.fingerprint != state.fingerprint {
			return fmt.Errorf("upstream configuration changed during probe; retry setup")
		}
		if err := rejectEndpointCollision(txApp, baseURL, current.source); err != nil {
			return err
		}
		var source *core.Record
		var endpoint *core.Record
		if current.source == nil {
			collection, err := txApp.FindCollectionByNameOrId(upstreamSources)
			if err != nil {
				return err
			}
			source = core.NewRecord(collection)
			endpointCollection, err := txApp.FindCollectionByNameOrId(upstreamEndpoints)
			if err != nil {
				return err
			}
			endpoint = core.NewRecord(endpointCollection)
			endpoint.Set("key", primaryEndpointKey)
		} else {
			source = current.source
			endpoint, err = activeEndpoint(current.endpoints)
			if err != nil {
				return err
			}
			if source.GetString("server_id") != probe.serverID {
				return fmt.Errorf("refusing to replace stored upstream server")
			}
		}
		now := time.Now().UTC()
		if err := ctx.Err(); err != nil {
			return err
		}
		source.Set("key", defaultUpstreamKey)
		source.Set("server_id", probe.serverID)
		source.Set("server_name", probe.serverName)
		source.Set("server_version", probe.version)
		source.Set("version_checked_at", now)
		source.Set("backend_username", opts.BackendUsername)
		source.Set("backend_password", opts.BackendPassword)
		source.Set("backend_user_id", probe.userID)
		source.Set("backend_token", probe.token)
		source.Set("token_updated_at", now)
		source.Set("last_login_at", now)
		source.Set("last_login_error", "")
		source.Set("backend_user_agent", identity.UserAgent)
		source.Set("backend_authorization_client", identity.Client)
		source.Set("backend_authorization_device", identity.Device)
		source.Set("backend_authorization_device_id", deviceID)
		source.Set("backend_authorization_version", identity.Version)
		if err := txApp.Save(source); err != nil {
			return err
		}
		if afterUpstreamSourceSave != nil {
			afterUpstreamSourceSave()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		endpoint.Set("source", source.Id)
		endpoint.Set("base_url", baseURL)
		endpoint.Set("active", true)
		return txApp.Save(endpoint)
	})
	if err != nil {
		logoutUpstream(ctx, baseURL, identity, deviceID, probe.userID, probe.token)
		return err
	}
	committed = true
	if committed && oldToken != "" && oldToken != probe.token {
		logoutUpstream(ctx, oldURL, oldIdentity, deviceID, oldUserID, oldToken)
	}
	return nil
}

func completeNoop(source, endpoint *core.Record, baseURL string, opts upstreamOptions, identity gateway.BackendClientIdentity) bool {
	return endpoint.GetString("base_url") == baseURL && source.GetString("backend_username") == opts.BackendUsername && source.GetString("backend_password") == opts.BackendPassword && source.GetString("backend_token") != "" && source.GetString("backend_user_id") != "" && source.GetString("server_id") != "" && source.GetString("backend_user_agent") == identity.UserAgent && source.GetString("backend_authorization_client") == identity.Client && source.GetString("backend_authorization_device") == identity.Device && source.GetString("backend_authorization_version") == identity.Version
}

func rejectEndpointCollision(app core.App, baseURL string, source *core.Record) error {
	records, err := app.FindRecordsByFilter(upstreamEndpoints, "base_url = {:url}", "", 0, 0, dbx.Params{"url": baseURL})
	if err != nil {
		return err
	}
	for _, record := range records {
		if source == nil || record.GetString("source") != source.Id || !record.GetBool("active") {
			return fmt.Errorf("refusing setup: target URL is owned by another endpoint")
		}
	}
	return nil
}

func logoutUpstream(ctx context.Context, baseURL string, identity gateway.BackendClientIdentity, deviceID, userID, token string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupGraceTimeout)
	defer cancel()
	err := upstreamRequest(cleanupCtx, newUpstreamHTTPClient(), http.MethodPost, upstreamURL(baseURL, "/Sessions/Logout"), nil, identity, deviceID, userID, token, &struct{}{}, true)
	if err != nil {
		fmt.Printf("warning: could not log out replaced upstream token: %v\n", err)
	}
}
