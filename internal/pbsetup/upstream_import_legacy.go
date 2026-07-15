package pbsetup

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"
	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
)

const importSummaryVersion = 1

var (
	marshalImportSnapshot    = json.Marshal
	marshalImportSummary     = json.Marshal
	readImportSummaryRecords = func(app core.App, collection string) ([]*core.Record, error) {
		return app.FindRecordsByFilter(collection, "", "id", 0, 0, nil)
	}
)

var afterImportPlanReread func()
var afterImportSourceSave func()

type importLegacyOptions struct {
	ServerRecordID, AccountRecordID string
	Apply                           bool
}

func newUpstreamImportLegacyCommand(app core.App) *cobra.Command {
	var opts importLegacyOptions
	cmd := &cobra.Command{Use: "import-legacy", Short: "Validate and import one explicit legacy upstream account", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(opts.ServerRecordID) == "" || strings.TrimSpace(opts.AccountRecordID) == "" {
				return fmt.Errorf("--server-record-id and --account-record-id are required")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			if err := app.Bootstrap(); err != nil {
				return err
			}
			_, data, err := runUpstreamImportLegacyPrepared(ctx, app, opts, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(append(data, '\n'))
			return err
		}}
	cmd.Flags().StringVar(&opts.ServerRecordID, "server-record-id", "", "Legacy emby_servers record ID")
	cmd.Flags().StringVar(&opts.AccountRecordID, "account-record-id", "", "Legacy backend_accounts record ID")
	cmd.Flags().BoolVar(&opts.Apply, "apply", false, "Persist a create or token repair after validation")
	_ = cmd.MarkFlagRequired("server-record-id")
	_ = cmd.MarkFlagRequired("account-record-id")
	return cmd
}

type importPlan struct {
	app                                                            core.App
	server, account                                                *core.Record
	state                                                          upstreamState
	url, deviceID, deviceSource                                    string
	identity                                                       gateway.BackendClientIdentity
	fingerprint                                                    string
	enabledUsers, eligibleUsers, enabledMappings, selectedMappings int
}

func runUpstreamImportLegacy(ctx context.Context, app core.App, opts importLegacyOptions, stderr interface{ Write([]byte) (int, error) }) (importSummary, error) {
	summary, _, err := runUpstreamImportLegacyPrepared(ctx, app, opts, stderr)
	return summary, err
}

func runUpstreamImportLegacyPrepared(ctx context.Context, app core.App, opts importLegacyOptions, stderr interface{ Write([]byte) (int, error) }) (importSummary, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	plan, err := loadImportPlan(app, opts)
	if err != nil {
		return importSummary{}, nil, err
	}
	client := newUpstreamHTTPClient()
	public, liveID, err := probeUpstreamPublic(ctx, client, plan.url, plan.deviceID, strings.TrimSpace(plan.server.GetString("server_id")), plan.identity)
	if err != nil {
		return importSummary{}, nil, err
	}
	action := importAction(plan, upstreamProbeResult{serverID: liveID})
	if action == "conflict" {
		return importSummary{}, nil, fmt.Errorf("singleton upstream configuration conflicts with legacy import")
	}
	if action == "noop" {
		summaryPlan := plan
		if plan.state.source != nil {
			summaryPlan.deviceID, summaryPlan.deviceSource = plan.state.source.GetString("backend_authorization_device_id"), "stored"
		}
		summary, err := makeImportSummary(app, summaryPlan, upstreamProbeResult{serverID: liveID}, action, opts.Apply, "not_created")
		if err != nil {
			return importSummary{}, nil, err
		}
		data, err := marshalImportSummary(summary)
		if err != nil {
			return importSummary{}, nil, err
		}
		return summary, data, nil
	}
	authDeviceID := plan.deviceID
	generation := ""
	if !opts.Apply {
		authDeviceID, err = newBackendDeviceID()
		if err != nil {
			return importSummary{}, nil, err
		}
	} else {
		authDeviceID, err = newBackendDeviceID()
		if err != nil {
			return importSummary{}, nil, err
		}
		generation, err = newAuthGenerationID()
		if err != nil {
			return importSummary{}, nil, err
		}
	}
	probe, err := authenticateUpstream(ctx, client, plan.url, plan.account.GetString("backend_username"), plan.account.GetString("backend_password"), authDeviceID, plan.identity, public, liveID)
	if err != nil {
		if probe.token != "" {
			if cleanupErr := cleanupImportNew(ctx, plan, probe, authDeviceID); cleanupErr != nil {
				return importSummary{}, nil, cleanupErr
			}
		}
		return importSummary{}, nil, err
	}
	owned, ownErr := classifyTokenOwnership(app, probe.token)
	if ownErr != nil || owned != tokenOwnershipInvocation {
		return importSummary{}, nil, ownErr
	}
	disposition := "persisted"
	if !opts.Apply {
		disposition = "logged_out"
	}
	summaryPlan := plan
	if opts.Apply {
		summaryPlan.deviceID, summaryPlan.deviceSource = authDeviceID, "generated"
	}
	summary, err := makeImportSummary(app, summaryPlan, probe, action, opts.Apply, disposition)
	if err != nil {
		if cleanupErr := cleanupImportNew(ctx, plan, probe, authDeviceID); cleanupErr != nil {
			return importSummary{}, nil, cleanupErr
		}
		return importSummary{}, nil, err
	}
	data, err := marshalImportSummary(summary)
	if err != nil {
		if cleanupErr := cleanupImportNew(ctx, plan, probe, authDeviceID); cleanupErr != nil {
			return importSummary{}, nil, cleanupErr
		}
		return importSummary{}, nil, err
	}
	if !opts.Apply {
		if err := cleanupImportNew(ctx, plan, probe, authDeviceID); err != nil {
			return importSummary{}, nil, fmt.Errorf("validation token cleanup failed")
		}
		return summary, data, nil
	}
	if err := ctx.Err(); err != nil {
		if cleanupErr := cleanupImportNew(ctx, plan, probe, authDeviceID); cleanupErr != nil {
			return importSummary{}, nil, cleanupErr
		}
		return importSummary{}, nil, err
	}
	var oldToken, oldURL, oldUserID, oldDeviceID, oldGeneration string
	var oldIdentity gateway.BackendClientIdentity
	err = app.RunInTransaction(func(tx core.App) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := loadImportPlan(tx, opts)
		if err != nil {
			return err
		}
		owned, ownErr := classifyTokenOwnership(tx, probe.token)
		if ownErr != nil || owned != tokenOwnershipInvocation {
			return ownErr
		}
		if current.fingerprint != plan.fingerprint || importAction(current, probe) != action {
			return fmt.Errorf("legacy import state changed during validation")
		}
		if afterImportPlanReread != nil {
			afterImportPlanReread()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if action == "create" {
			sources, err := tx.FindCollectionByNameOrId(upstreamSources)
			if err != nil {
				return err
			}
			source := core.NewRecord(sources)
			if err := ctx.Err(); err != nil {
				return err
			}
			setImportedSource(source, current, probe, time.Now().UTC(), authDeviceID, generation)
			if err := tx.Save(source); err != nil {
				return err
			}
			if afterImportSourceSave != nil {
				afterImportSourceSave()
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			endpoints, err := tx.FindCollectionByNameOrId(upstreamEndpoints)
			if err != nil {
				return err
			}
			endpoint := core.NewRecord(endpoints)
			if err := ctx.Err(); err != nil {
				return err
			}
			endpoint.Set("source", source.Id)
			endpoint.Set("key", primaryEndpointKey)
			endpoint.Set("base_url", current.url)
			endpoint.Set("active", true)
			return tx.Save(endpoint)
		}
		source := current.state.source
		endpoint, err := activeEndpoint(current.state.endpoints)
		if err != nil {
			return err
		}
		oldToken, oldURL, oldUserID, oldDeviceID, oldGeneration = source.GetString("backend_token"), endpoint.GetString("base_url"), source.GetString("backend_user_id"), source.GetString("backend_authorization_device_id"), source.GetString("auth_generation_id")
		oldIdentity = gateway.BackendClientIdentity{UserAgent: source.GetString("backend_user_agent"), Client: source.GetString("backend_authorization_client"), Device: source.GetString("backend_authorization_device"), Version: source.GetString("backend_authorization_version")}.WithDefaults()
		if err := ctx.Err(); err != nil {
			return err
		}
		now := time.Now().UTC()
		source.Set("backend_user_id", probe.userID)
		source.Set("backend_token", probe.token)
		source.Set("token_updated_at", now)
		source.Set("last_login_at", now)
		source.Set("last_login_error", "")
		source.Set("server_name", probe.serverName)
		source.Set("server_version", probe.version)
		source.Set("version_checked_at", now)
		source.Set("backend_authorization_device_id", authDeviceID)
		source.Set("auth_generation_id", generation)
		return tx.Save(source)
	})
	if err != nil {
		if isTokenOwnershipError(err) {
			return importSummary{}, nil, err
		}
		if cleanupErr := cleanupImportNew(ctx, plan, probe, authDeviceID); cleanupErr != nil {
			return importSummary{}, nil, cleanupErr
		}
		return importSummary{}, nil, err
	}
	if action == "repair-token" && oldToken != "" && oldGeneration != "" && oldToken != probe.token {
		owned, ownershipErr := classifyTokenOwnership(app, oldToken)
		if ownershipErr == nil && owned == tokenOwnershipInvocation {
			oldPlan := plan
			oldPlan.url, oldPlan.identity = oldURL, oldIdentity
			if err := logoutImport(ctx, oldPlan, upstreamProbeResult{token: oldToken, userID: oldUserID}, oldDeviceID); err != nil {
				_, _ = fmt.Fprintln(stderr, "warning: old singleton token cleanup failed")
			}
		}
	}
	return summary, data, nil
}

func loadImportPlan(app core.App, opts importLegacyOptions) (importPlan, error) {
	var p importPlan
	server, err := app.FindRecordById("emby_servers", strings.TrimSpace(opts.ServerRecordID))
	if err != nil {
		return p, fmt.Errorf("selected legacy server not found")
	}
	account, err := app.FindRecordById("backend_accounts", strings.TrimSpace(opts.AccountRecordID))
	if err != nil {
		return p, fmt.Errorf("selected legacy account not found")
	}
	if !server.GetBool("enabled") || !account.GetBool("enabled") || account.GetString("server") != server.Id {
		return p, fmt.Errorf("selected legacy records are not an enabled exact relation")
	}
	url, err := normalizeUpstreamURL(server.GetString("base_url"))
	if err != nil {
		return p, err
	}
	if strings.TrimSpace(account.GetString("backend_username")) == "" || strings.TrimSpace(account.GetString("backend_password")) == "" {
		return p, fmt.Errorf("selected legacy account has empty credentials")
	}
	p.app, p.server, p.account, p.url = app, server, account, url
	p.identity = gateway.BackendClientIdentity{UserAgent: server.GetString("backend_user_agent"), Client: server.GetString("backend_authorization_client"), Device: server.GetString("backend_authorization_device"), Version: server.GetString("backend_authorization_version")}.WithDefaults()
	p.deviceID = strings.TrimSpace(server.GetString("backend_authorization_device_id"))
	p.deviceSource = "stored"
	if p.deviceID == "" {
		p.deviceID, p.deviceSource = gateway.StableBackendDeviceID(server.Id), "stable-server-id"
	}
	p.state, err = loadImportSingleton(app)
	if err != nil {
		return p, err
	}
	users, err := app.FindRecordsByFilter("users", "enabled = true", "id", 0, 0, nil)
	if err != nil {
		return p, err
	}
	p.enabledUsers = len(users)
	maps, err := app.FindRecordsByFilter("user_mappings", "enabled = true", "id", 0, 0, nil)
	if err != nil {
		return p, err
	}
	offenders := []string{}
	for _, user := range users {
		n, selected := 0, false
		for _, m := range maps {
			if m.GetString("gateway_user") == user.Id {
				n++
				selected = selected || m.GetString("backend_account") == account.Id
			}
		}
		if n != 1 || !selected {
			offenders = append(offenders, user.Id)
		} else {
			p.eligibleUsers++
		}
	}
	for _, m := range maps {
		if m.GetString("backend_account") == account.Id {
			p.selectedMappings++
		}
	}
	p.enabledMappings = len(maps)
	if len(offenders) > 0 || p.eligibleUsers != p.enabledUsers {
		sort.Strings(offenders)
		return p, fmt.Errorf("enabled users lack exactly one selected-account mapping: %s", strings.Join(offenders, ","))
	}
	p.fingerprint, err = importFingerprint(p, maps, users)
	if err != nil {
		return p, err
	}
	return p, nil
}

func loadImportSingleton(app core.App) (upstreamState, error) {
	s := upstreamState{}
	source, err := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
	if err == nil {
		s.source = source
	} else if !isNotFound(err) {
		return s, err
	}
	all, err := app.FindRecordsByFilter(upstreamEndpoints, "", "id", 0, 0, nil)
	if err != nil {
		return s, err
	}
	s.allEndpoints = all
	s.endpoints = all
	if s.source != nil {
		s.endpoints, err = app.FindRecordsByFilter(upstreamEndpoints, "source = {:source}", "id", 0, 0, dbx.Params{"source": s.source.Id})
		if err != nil {
			return s, err
		}
	}
	s.fingerprint = upstreamFingerprint(s)
	return s, nil
}

func importFingerprint(p importPlan, maps, users []*core.Record) (string, error) {
	type serverSnapshot struct {
		ID, BaseURL, ServerID, UserAgent, Client, Device, DeviceID, Version string
		Enabled                                                             bool
	}
	type accountSnapshot struct {
		ID, Server, Username, Password string
		Enabled                        bool
	}
	type userSnapshot struct {
		ID      string
		Enabled bool
	}
	type mappingSnapshot struct {
		ID, GatewayUser, BackendAccount string
		Enabled                         bool
	}
	type sourceSnapshot struct{ ID, Key, ServerID, ServerName, ServerVersion, VersionCheckedAt, Username, Password, UserID, Token, AuthGenerationID, TokenUpdatedAt, LastLoginAt, LastLoginError, UserAgent, Client, Device, DeviceID, Version, Created, Updated string }
	type endpointSnapshot struct {
		ID, Source, Key, BaseURL, Created, Updated string
		Active                                     bool
	}
	type snapshot struct {
		Server    serverSnapshot
		Account   accountSnapshot
		Users     []userSnapshot
		Mappings  []mappingSnapshot
		Source    *sourceSnapshot
		Endpoints []endpointSnapshot
	}
	s := snapshot{Server: serverSnapshot{p.server.Id, p.server.GetString("base_url"), p.server.GetString("server_id"), p.server.GetString("backend_user_agent"), p.server.GetString("backend_authorization_client"), p.server.GetString("backend_authorization_device"), p.server.GetString("backend_authorization_device_id"), p.server.GetString("backend_authorization_version"), p.server.GetBool("enabled")}, Account: accountSnapshot{p.account.Id, p.account.GetString("server"), p.account.GetString("backend_username"), p.account.GetString("backend_password"), p.account.GetBool("enabled")}}
	for _, r := range users {
		s.Users = append(s.Users, userSnapshot{r.Id, r.GetBool("enabled")})
	}
	for _, r := range maps {
		s.Mappings = append(s.Mappings, mappingSnapshot{r.Id, r.GetString("gateway_user"), r.GetString("backend_account"), r.GetBool("enabled")})
	}
	if r := p.state.source; r != nil {
		s.Source = &sourceSnapshot{r.Id, r.GetString("key"), r.GetString("server_id"), r.GetString("server_name"), r.GetString("server_version"), r.GetDateTime("version_checked_at").String(), r.GetString("backend_username"), r.GetString("backend_password"), r.GetString("backend_user_id"), r.GetString("backend_token"), r.GetString("auth_generation_id"), r.GetDateTime("token_updated_at").String(), r.GetDateTime("last_login_at").String(), r.GetString("last_login_error"), r.GetString("backend_user_agent"), r.GetString("backend_authorization_client"), r.GetString("backend_authorization_device"), r.GetString("backend_authorization_device_id"), r.GetString("backend_authorization_version"), r.GetDateTime("created").String(), r.GetDateTime("updated").String()}
	}
	for _, r := range p.state.allEndpoints {
		s.Endpoints = append(s.Endpoints, endpointSnapshot{r.Id, r.GetString("source"), r.GetString("key"), r.GetString("base_url"), r.GetDateTime("created").String(), r.GetDateTime("updated").String(), r.GetBool("active")})
	}
	sort.Slice(s.Users, func(i, j int) bool { return s.Users[i].ID < s.Users[j].ID })
	sort.Slice(s.Mappings, func(i, j int) bool { return s.Mappings[i].ID < s.Mappings[j].ID })
	sort.Slice(s.Endpoints, func(i, j int) bool { return s.Endpoints[i].ID < s.Endpoints[j].ID })
	b, err := marshalImportSnapshot(s)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

func importAction(p importPlan, probe upstreamProbeResult) string {
	if p.state.source == nil {
		if len(p.state.allEndpoints) == 0 {
			return "create"
		}
		return "conflict"
	}
	if len(p.state.allEndpoints) != 1 || len(p.state.endpoints) != 1 {
		return "conflict"
	}
	e := p.state.endpoints[0]
	s := p.state.source
	if !e.GetBool("active") || e.GetString("key") != primaryEndpointKey || e.GetString("base_url") != p.url || s.GetString("key") != defaultUpstreamKey || s.GetString("server_id") != probe.serverID || s.GetString("backend_username") != p.account.GetString("backend_username") || s.GetString("backend_password") != p.account.GetString("backend_password") || s.GetString("backend_user_agent") != p.identity.UserAgent || s.GetString("backend_authorization_client") != p.identity.Client || s.GetString("backend_authorization_device") != p.identity.Device || s.GetString("backend_authorization_version") != p.identity.Version {
		return "conflict"
	}
	if s.GetString("backend_token") == "" || s.GetString("backend_user_id") == "" || s.GetString("auth_generation_id") == "" {
		return "repair-token"
	}
	return "noop"
}

func setImportedSource(s *core.Record, p importPlan, probe upstreamProbeResult, now time.Time, deviceID, generation string) {
	s.Set("key", defaultUpstreamKey)
	s.Set("server_id", probe.serverID)
	s.Set("server_name", probe.serverName)
	s.Set("server_version", probe.version)
	s.Set("version_checked_at", now)
	s.Set("backend_username", p.account.GetString("backend_username"))
	s.Set("backend_password", p.account.GetString("backend_password"))
	s.Set("backend_user_id", probe.userID)
	s.Set("backend_token", probe.token)
	s.Set("token_updated_at", now)
	s.Set("last_login_at", now)
	s.Set("last_login_error", "")
	s.Set("backend_user_agent", p.identity.UserAgent)
	s.Set("backend_authorization_client", p.identity.Client)
	s.Set("backend_authorization_device", p.identity.Device)
	s.Set("backend_authorization_device_id", deviceID)
	s.Set("backend_authorization_version", p.identity.Version)
	s.Set("auth_generation_id", generation)
}

func cleanupImportNew(ctx context.Context, p importPlan, result upstreamProbeResult, deviceID string) error {
	if result.token == "" {
		return nil
	}
	owned, err := classifyTokenOwnership(p.app, result.token)
	if err != nil || owned != tokenOwnershipInvocation {
		return err
	}
	return logoutImport(ctx, p, result, deviceID)
}

func logoutImport(ctx context.Context, p importPlan, result upstreamProbeResult, deviceID string) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupGraceTimeout)
	defer cancel()
	return upstreamRequest(cleanup, newUpstreamHTTPClient(), http.MethodPost, upstreamURL(p.url, "/Sessions/Logout"), nil, p.identity, deviceID, result.userID, result.token, &struct{}{}, true)
}

type importSummary struct {
	SchemaVersion              int                       `json:"schema_version"`
	Mode                       string                    `json:"mode"`
	Action                     string                    `json:"action"`
	ServerRecordID             string                    `json:"server_record_id"`
	AccountRecordID            string                    `json:"account_record_id"`
	EndpointURL                string                    `json:"endpoint_url"`
	LiveServerID               string                    `json:"live_server_id"`
	EndpointKey                string                    `json:"endpoint_key"`
	DeviceIDSource             string                    `json:"device_id_source"`
	DeviceIDDisposition        string                    `json:"device_id_disposition"`
	DeviceIDFingerprint        string                    `json:"device_id_sha256"`
	IdentityDefaultsApplied    []string                  `json:"identity_defaults_applied"`
	SingletonState             string                    `json:"singleton_state"`
	EnabledUsers               int                       `json:"enabled_user_count"`
	EligibleUsers              int                       `json:"selected_account_eligible_user_count"`
	EnabledMappings            int                       `json:"enabled_mapping_count"`
	SelectedMappings           int                       `json:"selected_mapping_count"`
	UnrevokedSessions          int                       `json:"unrevoked_session_count"`
	SelectedAccountChildCounts int                       `json:"selected_account_child_count"`
	Collections                []importCollectionSummary `json:"collections"`
	ValidationToken            string                    `json:"validation_token_disposition"`
}

type importCollectionSummary struct {
	Name        string `json:"name"`
	Count       int    `json:"count"`
	Fingerprint string `json:"fingerprint"`
}

func makeImportSummary(app core.App, p importPlan, probe upstreamProbeResult, action string, apply bool, disposition string) (importSummary, error) {
	mode := "dry-run"
	if apply {
		mode = "apply"
	}
	collections, sessions, childCounts, err := importCollectionSummaries(app, p.account.Id)
	if err != nil {
		return importSummary{}, err
	}
	defaults := []string{}
	for _, pair := range []struct{ name, value string }{{"user_agent", p.server.GetString("backend_user_agent")}, {"client", p.server.GetString("backend_authorization_client")}, {"device", p.server.GetString("backend_authorization_device")}, {"version", p.server.GetString("backend_authorization_version")}} {
		if strings.TrimSpace(pair.value) == "" {
			defaults = append(defaults, pair.name)
		}
	}
	deviceDisposition, fingerprint := "stored", fmt.Sprintf("%x", sha256.Sum256([]byte(p.deviceID)))
	if !apply && action != "noop" {
		deviceDisposition, fingerprint = "generate_on_apply", ""
	}
	if apply && action != "noop" {
		deviceDisposition = "generated"
	}
	return importSummary{SchemaVersion: importSummaryVersion, Mode: mode, Action: action, ServerRecordID: p.server.Id, AccountRecordID: p.account.Id, EndpointURL: p.url, LiveServerID: probe.serverID, EndpointKey: primaryEndpointKey, DeviceIDSource: p.deviceSource, DeviceIDDisposition: deviceDisposition, DeviceIDFingerprint: fingerprint, IdentityDefaultsApplied: defaults, SingletonState: stateName(p.state), EnabledUsers: p.enabledUsers, EligibleUsers: p.eligibleUsers, EnabledMappings: p.enabledMappings, SelectedMappings: p.selectedMappings, UnrevokedSessions: sessions, SelectedAccountChildCounts: childCounts, Collections: collections, ValidationToken: disposition}, nil
}

func importCollectionSummaries(app core.App, accountID string) ([]importCollectionSummary, int, int, error) {
	names := []string{"users", "emby_servers", "backend_accounts", "user_mappings", "gateway_sessions", "user_item_data", "playback_events", "display_preferences", "audit_logs", "item_child_counts", "path_policies"}
	result := make([]importCollectionSummary, 0, len(names))
	unrevoked, childCounts := 0, 0
	for _, name := range names {
		records, err := readImportSummaryRecords(app, name)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("read preservation collection %s: %w", name, err)
		}
		parts := make([]string, 0, len(records))
		for _, r := range records {
			parts = append(parts, strings.Join([]string{r.Id, r.GetDateTime("created").String(), r.GetDateTime("updated").String()}, "\x00"))
			if name == "gateway_sessions" && r.GetDateTime("revoked_at").IsZero() {
				unrevoked++
			}
			if name == "item_child_counts" && r.GetString("backend_account_id") == accountID {
				childCounts++
			}
		}
		sort.Strings(parts)
		result = append(result, importCollectionSummary{Name: name, Count: len(records), Fingerprint: fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(parts, "\x01"))))})
	}
	return result, unrevoked, childCounts, nil
}
func stateName(s upstreamState) string {
	if s.source == nil {
		if len(s.allEndpoints) == 0 {
			return "empty"
		}
		return "orphan-endpoints"
	}
	return "present"
}
