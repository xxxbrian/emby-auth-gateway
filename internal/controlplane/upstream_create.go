package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

const (
	DefaultUpstreamKey = "default"
	PrimaryEndpointKey = "primary"
	UpstreamSources    = "upstream_sources"
	UpstreamEndpoints  = "upstream_endpoints"
)

// Test hooks for deterministic cancellation/drift tests.
var (
	AfterUpstreamProbe      func()
	AfterUpstreamSourceSave func()
	ReadCurrentTokenSource  = func(app core.App) (*core.Record, error) {
		return app.FindFirstRecordByData(UpstreamSources, "key", DefaultUpstreamKey)
	}
	LoadUpstreamStateForCreate = LoadUpstreamState
)

// UpstreamReconfigureInput configures the singleton upstream source.
type UpstreamReconfigureInput struct {
	EmbyBaseURL                 string
	BackendUsername             string
	BackendPassword             string
	BackendUserAgent            string
	BackendAuthorizationClient  string
	BackendAuthorizationDevice  string
	BackendAuthorizationVersion string
	AllowCreate                 bool // CLI true; admin false
	Force                       bool // admin force despite active media (admin layer checks media; pass through for logging)
}

// UpstreamReconfigureResult holds non-secret outcomes of reconfiguration.
type UpstreamReconfigureResult struct {
	CleanupWarning string // structured non-secret warning if old token cleanup failed
}

func (o UpstreamReconfigureInput) identity() gateway.BackendClientIdentity {
	return gateway.BackendClientIdentity{
		UserAgent: o.BackendUserAgent,
		Client:    o.BackendAuthorizationClient,
		Device:    o.BackendAuthorizationDevice,
		Version:   o.BackendAuthorizationVersion,
	}.WithDefaults()
}

func protectedTokens(app core.App) (map[string]struct{}, error) {
	set := map[string]struct{}{}
	source, err := ReadCurrentTokenSource(app)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		if token := source.GetString("backend_token"); token != "" {
			set[token] = struct{}{}
		}
	}
	return set, nil
}

// TokenOwnership classifies whether a token is protected (current source) or invocation-owned.
type TokenOwnership int

const (
	TokenOwnershipUnknown TokenOwnership = iota
	TokenOwnershipProtected
	TokenOwnershipInvocation
)

type tokenOwnershipError struct {
	outcome TokenOwnership
	cause   error
}

func (e *tokenOwnershipError) Error() string { return "upstream token ownership cannot be established" }

func (e *tokenOwnershipError) Unwrap() error { return e.cause }

// ClassifyTokenOwnership reports whether token is the current protected source token.
func ClassifyTokenOwnership(app core.App, token string) (TokenOwnership, error) {
	if token == "" {
		return TokenOwnershipUnknown, nil
	}
	set, err := protectedTokens(app)
	if err != nil {
		return TokenOwnershipUnknown, &tokenOwnershipError{outcome: TokenOwnershipUnknown, cause: err}
	}
	if _, found := set[token]; found {
		return TokenOwnershipProtected, &tokenOwnershipError{outcome: TokenOwnershipProtected}
	}
	return TokenOwnershipInvocation, nil
}

func cleanupInvocationToken(ctx context.Context, app core.App, baseURL string, identity gateway.BackendClientIdentity, deviceID, userID, token string) error {
	if token == "" {
		return nil
	}
	owned, err := ClassifyTokenOwnership(app, token)
	if err != nil || owned != TokenOwnershipInvocation {
		return err
	}
	_ = logoutUpstream(ctx, baseURL, identity, deviceID, userID, token)
	return nil
}

// IsTokenOwnershipError reports whether err is a token ownership failure.
func IsTokenOwnershipError(err error) bool {
	var ownershipErr *tokenOwnershipError
	return errors.As(err, &ownershipErr)
}

func newAuthGenerationID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", value), nil
}

// UpstreamState is a snapshot of singleton upstream configuration.
type UpstreamState struct {
	Source       *core.Record
	Endpoints    []*core.Record
	AllEndpoints []*core.Record
	Fingerprint  string
}

// LoadUpstreamState loads the default upstream source and related endpoints.
func LoadUpstreamState(app core.App) (UpstreamState, error) {
	state := UpstreamState{}
	source, err := app.FindFirstRecordByData(UpstreamSources, "key", DefaultUpstreamKey)
	if err == nil {
		state.Source = source
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state, err
	}
	endpoints, err := app.FindRecordsByFilter(UpstreamEndpoints, "", "id", 0, 0, nil)
	if err != nil {
		return state, err
	}
	state.AllEndpoints = endpoints
	state.Endpoints = endpoints
	if state.Source == nil && len(endpoints) > 0 {
		return state, fmt.Errorf("refusing setup: orphan upstream endpoint rows exist")
	}
	if state.Source != nil {
		state.Endpoints, err = app.FindRecordsByFilter(UpstreamEndpoints, "source = {:source}", "id", 0, 0, dbx.Params{"source": state.Source.Id})
		if err != nil {
			return state, err
		}
	}
	state.Fingerprint = UpstreamFingerprint(state)
	return state, nil
}

// UpstreamFingerprint returns a stable hash of upstream configuration for drift detection.
func UpstreamFingerprint(state UpstreamState) string {
	type sourceSnapshot struct {
		ID, Key, ServerID, ServerName, ServerVersion, VersionCheckedAt, BackendUsername, BackendPassword, BackendUserID, BackendToken, AuthGenerationID, TokenUpdatedAt, LastLoginAt, LastLoginError, UserAgent, Client, Device, DeviceID, Version, Updated string
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
	if state.Source != nil {
		s.Source = &sourceSnapshot{state.Source.Id, state.Source.GetString("key"), state.Source.GetString("server_id"), state.Source.GetString("server_name"), state.Source.GetString("server_version"), state.Source.GetDateTime("version_checked_at").String(), state.Source.GetString("backend_username"), state.Source.GetString("backend_password"), state.Source.GetString("backend_user_id"), state.Source.GetString("backend_token"), state.Source.GetString("auth_generation_id"), state.Source.GetDateTime("token_updated_at").String(), state.Source.GetDateTime("last_login_at").String(), state.Source.GetString("last_login_error"), state.Source.GetString("backend_user_agent"), state.Source.GetString("backend_authorization_client"), state.Source.GetString("backend_authorization_device"), state.Source.GetString("backend_authorization_device_id"), state.Source.GetString("backend_authorization_version"), state.Source.GetDateTime("updated").String()}
	}
	for _, endpoint := range state.AllEndpoints {
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

// ReconfigureUpstream creates or updates the singleton upstream configuration.
func ReconfigureUpstream(parent context.Context, app core.App, opts UpstreamReconfigureInput) (UpstreamReconfigureResult, error) {
	result := UpstreamReconfigureResult{}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	baseURL, err := NormalizeUpstreamURL(opts.EmbyBaseURL)
	if err != nil {
		return result, err
	}
	state, err := LoadUpstreamStateForCreate(app)
	if err != nil {
		return result, err
	}
	if state.Source == nil && !opts.AllowCreate {
		return result, fmt.Errorf("upstream source does not exist; create is not allowed")
	}
	identity := opts.identity()
	var deviceID string
	var active *core.Record
	var oldToken, oldURL, oldUserID, oldDeviceID, oldGeneration string
	var oldIdentity gateway.BackendClientIdentity
	if state.Source != nil {
		active, err = activeEndpoint(state.Endpoints)
		if err != nil {
			return result, err
		}
		deviceID = state.Source.GetString("backend_authorization_device_id")
		if deviceID == "" {
			return result, fmt.Errorf("refusing setup: stored source has no device ID")
		}
		oldToken, oldURL, oldUserID, oldDeviceID, oldGeneration = state.Source.GetString("backend_token"), active.GetString("base_url"), state.Source.GetString("backend_user_id"), state.Source.GetString("backend_authorization_device_id"), state.Source.GetString("auth_generation_id")
		oldIdentity = gateway.BackendClientIdentity{UserAgent: state.Source.GetString("backend_user_agent"), Client: state.Source.GetString("backend_authorization_client"), Device: state.Source.GetString("backend_authorization_device"), Version: state.Source.GetString("backend_authorization_version")}.WithDefaults()
		if err := rejectEndpointCollision(app, baseURL, state.Source); err != nil {
			return result, err
		}
	}
	exactNoop := state.Source != nil && completeNoop(state.Source, active, baseURL, opts, identity)
	if state.Source == nil {
		if err := rejectEndpointCollision(app, baseURL, nil); err != nil {
			return result, err
		}
	}
	expectedID := ""
	if state.Source != nil {
		expectedID = state.Source.GetString("server_id")
	}
	if exactNoop {
		_, _, err := probeUpstreamPublic(ctx, NewUpstreamHTTPClient(), baseURL, state.Source.GetString("backend_authorization_device_id"), expectedID, identity)
		return result, err
	}
	deviceID, err = newBackendDeviceID()
	if err != nil {
		return result, err
	}
	generation, err := newAuthGenerationID()
	if err != nil {
		return result, err
	}
	probe, err := probeUpstream(ctx, baseURL, opts.BackendUsername, opts.BackendPassword, deviceID, expectedID, identity)
	if err != nil {
		if probe.token != "" {
			if cleanupErr := cleanupInvocationToken(ctx, app, baseURL, identity, deviceID, probe.userID, probe.token); cleanupErr != nil {
				return result, cleanupErr
			}
		}
		return result, err
	}
	owned, ownErr := ClassifyTokenOwnership(app, probe.token)
	if ownErr != nil || owned != TokenOwnershipInvocation {
		return result, ownErr
	}
	if AfterUpstreamProbe != nil {
		AfterUpstreamProbe()
	}
	if err := ctx.Err(); err != nil {
		if cleanupErr := cleanupInvocationToken(ctx, app, baseURL, identity, deviceID, probe.userID, probe.token); cleanupErr != nil {
			return result, cleanupErr
		}
		return result, err
	}
	err = app.RunInTransaction(func(txApp core.App) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := LoadUpstreamStateForCreate(txApp)
		if err != nil {
			return err
		}
		owned, ownErr := ClassifyTokenOwnership(txApp, probe.token)
		if ownErr != nil || owned != TokenOwnershipInvocation {
			return ownErr
		}
		if current.Fingerprint != state.Fingerprint {
			return fmt.Errorf("upstream configuration changed during probe; retry setup")
		}
		if err := rejectEndpointCollision(txApp, baseURL, current.Source); err != nil {
			return err
		}
		var source *core.Record
		var endpoint *core.Record
		if current.Source == nil {
			if !opts.AllowCreate {
				return fmt.Errorf("upstream source does not exist; create is not allowed")
			}
			collection, err := txApp.FindCollectionByNameOrId(UpstreamSources)
			if err != nil {
				return err
			}
			source = core.NewRecord(collection)
			endpointCollection, err := txApp.FindCollectionByNameOrId(UpstreamEndpoints)
			if err != nil {
				return err
			}
			endpoint = core.NewRecord(endpointCollection)
			endpoint.Set("key", PrimaryEndpointKey)
		} else {
			source = current.Source
			endpoint, err = activeEndpoint(current.Endpoints)
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
		source.Set("key", DefaultUpstreamKey)
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
		source.Set("auth_generation_id", generation)
		if err := txApp.Save(source); err != nil {
			return err
		}
		if AfterUpstreamSourceSave != nil {
			AfterUpstreamSourceSave()
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
		if IsTokenOwnershipError(err) {
			return result, err
		}
		if cleanupErr := cleanupInvocationToken(ctx, app, baseURL, identity, deviceID, probe.userID, probe.token); cleanupErr != nil {
			return result, cleanupErr
		}
		return result, err
	}
	if oldToken != "" && oldGeneration != "" && oldToken != probe.token {
		owned, err := ClassifyTokenOwnership(app, oldToken)
		if err == nil && owned == TokenOwnershipInvocation {
			if logoutErr := logoutUpstream(ctx, oldURL, oldIdentity, oldDeviceID, oldUserID, oldToken); logoutErr != nil {
				result.CleanupWarning = fmt.Sprintf("could not log out replaced upstream token: %v", logoutErr)
			}
		}
	}
	_ = opts.Force // reserved for admin layer; no behavior change in shared path
	return result, nil
}

func completeNoop(source, endpoint *core.Record, baseURL string, opts UpstreamReconfigureInput, identity gateway.BackendClientIdentity) bool {
	return endpoint.GetString("base_url") == baseURL && source.GetString("backend_username") == opts.BackendUsername && source.GetString("backend_password") == opts.BackendPassword && source.GetString("backend_token") != "" && source.GetString("backend_user_id") != "" && source.GetString("auth_generation_id") != "" && source.GetString("server_id") != "" && source.GetString("backend_user_agent") == identity.UserAgent && source.GetString("backend_authorization_client") == identity.Client && source.GetString("backend_authorization_device") == identity.Device && source.GetString("backend_authorization_version") == identity.Version
}

func rejectEndpointCollision(app core.App, baseURL string, source *core.Record) error {
	records, err := app.FindRecordsByFilter(UpstreamEndpoints, "base_url = {:url}", "", 0, 0, dbx.Params{"url": baseURL})
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

func logoutUpstream(ctx context.Context, baseURL string, identity gateway.BackendClientIdentity, deviceID, userID, token string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupGraceTimeout)
	defer cancel()
	return UpstreamRequest(cleanupCtx, NewUpstreamHTTPClient(), http.MethodPost, upstreamURL(baseURL, "/Sessions/Logout"), nil, identity, deviceID, userID, token, &struct{}{}, true)
}
