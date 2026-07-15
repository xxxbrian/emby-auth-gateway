package pbstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

type Store struct {
	app core.App
}

const playbackStateItemIDBatchLimit = 50

func New(app core.App) *Store {
	return &Store{app: app}
}

func (s *Store) LoadDefaultUpstreamRuntime(ctx context.Context) (*gateway.UpstreamRuntime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var runtime *gateway.UpstreamRuntime
	err := s.app.RunInTransaction(func(tx core.App) error {
		var err error
		runtime, err = loadDefaultUpstreamRuntime(ctx, tx)
		if err != nil {
			return err
		}
		return ctx.Err()
	})
	if err != nil {
		return nil, classifyUpstreamStoreError(ctx, err)
	}
	return runtime, nil
}

func (s *Store) CompareAndSwapUpstreamAuth(ctx context.Context, update gateway.UpstreamAuthUpdate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := gateway.ValidateUpstreamAuthUpdate(update); err != nil {
		return err
	}
	err := s.app.RunInTransaction(func(tx core.App) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		at := update.AuthenticatedAt.UTC()
		result, err := tx.DB().NewQuery(`UPDATE upstream_sources
			SET auth_generation_id = {:generationID}, backend_authorization_device_id = {:deviceID}, backend_user_id = {:backendUserID}, backend_token = {:backendToken}, token_updated_at = {:authenticatedAt}, last_login_at = {:authenticatedAt}, last_login_error = ''
			WHERE id = {:sourceID} AND key = 'default' AND auth_generation_id = {:expectedGenerationID}`).
			WithContext(ctx).
			Bind(dbx.Params{
				"generationID": update.GenerationID, "deviceID": update.DeviceID, "backendUserID": update.BackendUserID,
				"backendToken": update.BackendToken, "authenticatedAt": at, "sourceID": update.SourceID,
				"expectedGenerationID": update.ExpectedGenerationID,
			}).Execute()
		if err != nil {
			return fmt.Errorf("%w: update upstream authentication: %v", gateway.ErrStoreUnavailable, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("%w: inspect upstream authentication update: %v", gateway.ErrStoreUnavailable, err)
		}
		if affected == 1 {
			return ctx.Err()
		}
		runtime, loadErr := loadDefaultUpstreamRuntime(ctx, tx)
		if loadErr != nil {
			return loadErr
		}
		if runtime.Source.ID != update.SourceID {
			return gateway.ErrUpstreamNotFound
		}
		return gateway.ErrUpstreamAuthConflict
	})
	return classifyUpstreamStoreError(ctx, err)
}

func classifyUpstreamStoreError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || isGatewayDomainError(err) {
		return err
	}
	return fmt.Errorf("%w: %v", gateway.ErrStoreUnavailable, err)
}

func isGatewayDomainError(err error) bool {
	return errors.Is(err, gateway.ErrInvalidCredentials) || errors.Is(err, gateway.ErrNotFound) || errors.Is(err, gateway.ErrDisabled) || errors.Is(err, gateway.ErrUnauthorized) || errors.Is(err, gateway.ErrBadRequest) || errors.Is(err, gateway.ErrUpstreamNotFound) || errors.Is(err, gateway.ErrInvalidUpstreamTopology) || errors.Is(err, gateway.ErrUpstreamAuthConflict) || errors.Is(err, gateway.ErrStoreUnavailable)
}

func loadDefaultUpstreamRuntime(ctx context.Context, app core.App) (*gateway.UpstreamRuntime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := app.FindCollectionByNameOrId("upstream_sources"); err != nil {
		return nil, fmt.Errorf("%w: upstream_sources: %v", gateway.ErrStoreUnavailable, err)
	}
	if _, err := app.FindCollectionByNameOrId("upstream_endpoints"); err != nil {
		return nil, fmt.Errorf("%w: upstream_endpoints: %v", gateway.ErrStoreUnavailable, err)
	}
	sources, err := app.FindAllRecords("upstream_sources")
	if err != nil {
		return nil, fmt.Errorf("%w: load upstream sources: %v", gateway.ErrStoreUnavailable, err)
	}
	endpoints, err := app.FindAllRecords("upstream_endpoints")
	if err != nil {
		return nil, fmt.Errorf("%w: load upstream endpoints: %v", gateway.ErrStoreUnavailable, err)
	}
	if len(sources) == 0 && len(endpoints) == 0 {
		return nil, gateway.ErrUpstreamNotFound
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("%w: endpoints without source", gateway.ErrInvalidUpstreamTopology)
	}
	if len(sources) != 1 {
		return nil, fmt.Errorf("%w: expected one source", gateway.ErrInvalidUpstreamTopology)
	}
	source := upstreamSourceFromRecord(sources[0])
	var active []gateway.UpstreamEndpoint
	for _, record := range endpoints {
		endpoint := gateway.UpstreamEndpoint{ID: record.Id, SourceID: record.GetString("source"), Key: record.GetString("key"), BaseURL: record.GetString("base_url"), Active: record.GetBool("active")}
		if err := gateway.ValidateUpstreamEndpoint(source.ID, endpoint); err != nil {
			return nil, err
		}
		if endpoint.Active {
			active = append(active, endpoint)
		}
	}
	if len(active) != 1 {
		return nil, fmt.Errorf("%w: expected one active endpoint", gateway.ErrInvalidUpstreamTopology)
	}
	runtime := &gateway.UpstreamRuntime{Source: source, Endpoint: active[0]}
	if err := gateway.ValidateUpstreamRuntime(*runtime); err != nil {
		return nil, err
	}
	return runtime, nil
}

func upstreamSourceFromRecord(record *core.Record) gateway.UpstreamSource {
	var versionCheckedAt, tokenUpdatedAt, lastLoginAt *time.Time
	if !record.GetDateTime("version_checked_at").IsZero() {
		t := record.GetDateTime("version_checked_at").Time()
		versionCheckedAt = &t
	}
	if !record.GetDateTime("token_updated_at").IsZero() {
		t := record.GetDateTime("token_updated_at").Time()
		tokenUpdatedAt = &t
	}
	if !record.GetDateTime("last_login_at").IsZero() {
		t := record.GetDateTime("last_login_at").Time()
		lastLoginAt = &t
	}
	return gateway.UpstreamSource{
		ID: record.Id, Key: record.GetString("key"), ServerID: record.GetString("server_id"), ServerName: record.GetString("server_name"), ServerVersion: record.GetString("server_version"), VersionCheckedAt: versionCheckedAt,
		BackendUsername: record.GetString("backend_username"), BackendPassword: record.GetString("backend_password"), BackendUserID: record.GetString("backend_user_id"), BackendToken: record.GetString("backend_token"), AuthGenerationID: record.GetString("auth_generation_id"), TokenUpdatedAt: tokenUpdatedAt, LastLoginAt: lastLoginAt, LastLoginError: record.GetString("last_login_error"),
		ClientIdentity: gateway.BackendClientIdentity{UserAgent: record.GetString("backend_user_agent"), Client: record.GetString("backend_authorization_client"), Device: record.GetString("backend_authorization_device"), DeviceID: record.GetString("backend_authorization_device_id"), Version: record.GetString("backend_authorization_version")},
	}
}

func (s *Store) AuthenticateGatewayUser(ctx context.Context, username, password string) (*gateway.GatewayUser, error) {
	record, err := s.app.FindFirstRecordByData("users", "username", username)
	if err != nil {
		return nil, gateway.ErrInvalidCredentials
	}
	if !record.GetBool("enabled") || !record.ValidatePassword(password) {
		return nil, gateway.ErrInvalidCredentials
	}
	return userFromRecord(record), nil
}

func (s *Store) FindGatewayUserByUsername(ctx context.Context, username string) (*gateway.GatewayUser, error) {
	record, err := s.app.FindFirstRecordByData("users", "username", username)
	if err != nil {
		return nil, gateway.ErrNotFound
	}
	return userFromRecord(record), nil
}

func (s *Store) ListPublicUsers(ctx context.Context) ([]gateway.GatewayUser, error) {
	records, err := s.app.FindAllRecords("users", dbx.HashExp{"enabled": true})
	if err != nil {
		return nil, err
	}
	users := make([]gateway.GatewayUser, 0, len(records))
	for _, record := range records {
		users = append(users, *userFromRecord(record))
	}
	return users, nil
}

func (s *Store) FindUserBySyntheticID(ctx context.Context, syntheticID string) (*gateway.GatewayUser, error) {
	record, err := s.app.FindFirstRecordByData("users", "synthetic_user_id", syntheticID)
	if err != nil {
		return nil, gateway.ErrNotFound
	}
	if !record.GetBool("enabled") {
		return nil, gateway.ErrDisabled
	}
	return userFromRecord(record), nil
}

func (s *Store) FindMappingByGatewayUserID(ctx context.Context, gatewayUserID string) (*gateway.UserMapping, error) {
	mapping, err := s.app.FindFirstRecordByData("user_mappings", "gateway_user", gatewayUserID)
	if err != nil {
		return nil, gateway.ErrNotFound
	}
	if !mapping.GetBool("enabled") {
		return nil, gateway.ErrDisabled
	}
	backendID := mapping.GetString("backend_account")
	account, err := s.backendAccountByID(backendID)
	if err != nil {
		return nil, err
	}
	return &gateway.UserMapping{
		ID:               mapping.Id,
		GatewayUserID:    gatewayUserID,
		BackendAccountID: backendID,
		BackendAccount:   *account,
		Enabled:          mapping.GetBool("enabled"),
	}, nil
}

func (s *Store) FindBackendAccountByID(ctx context.Context, backendAccountID string) (*gateway.BackendAccount, error) {
	return s.backendAccountByID(backendAccountID)
}

func (s *Store) ListEnabledServers(ctx context.Context) ([]gateway.EmbyServer, error) {
	records, err := s.app.FindAllRecords("emby_servers", dbx.HashExp{"enabled": true})
	if err != nil {
		return nil, err
	}
	servers := make([]gateway.EmbyServer, 0, len(records))
	for _, record := range records {
		servers = append(servers, serverFromRecord(record))
	}
	return servers, nil
}

func (s *Store) UpdateBackendToken(ctx context.Context, accountID, token, backendUserID string, updatedAt time.Time) error {
	record, err := s.app.FindRecordById("backend_accounts", accountID)
	if err != nil {
		return gateway.ErrNotFound
	}
	record.Set("backend_token", token)
	record.Set("backend_user_id", backendUserID)
	record.Set("token_updated_at", updatedAt.UTC())
	record.Set("last_login_at", updatedAt.UTC())
	record.Set("last_login_error", "")
	return s.app.Save(record)
}

func (s *Store) RecordBackendLoginError(ctx context.Context, accountID, message string) error {
	record, err := s.app.FindRecordById("backend_accounts", accountID)
	if err != nil {
		return gateway.ErrNotFound
	}
	record.Set("last_login_error", message)
	return s.app.Save(record)
}

func (s *Store) UpdateServerInfo(ctx context.Context, serverRecordID, serverID, serverName, serverVersion string, checkedAt time.Time) error {
	record, err := s.app.FindRecordById("emby_servers", serverRecordID)
	if err != nil {
		return gateway.ErrNotFound
	}
	if strings.TrimSpace(serverID) != "" {
		record.Set("server_id", serverID)
	}
	if strings.TrimSpace(serverName) != "" {
		record.Set("server_name", serverName)
	}
	if strings.TrimSpace(serverVersion) != "" {
		record.Set("server_version", serverVersion)
	}
	record.Set("version_checked_at", checkedAt.UTC())
	return s.app.Save(record)
}

func (s *Store) RecordAudit(ctx context.Context, entry gateway.AuditLog) error {
	collection, err := s.app.FindCollectionByNameOrId("audit_logs")
	if err != nil {
		return err
	}
	record := core.NewRecord(collection)
	if entry.GatewayUserID != "" {
		record.Set("gateway_user", entry.GatewayUserID)
	}
	record.Set("synthetic_user_id", entry.SyntheticUserID)
	record.Set("event", entry.Event)
	record.Set("message", entry.Message)
	record.Set("remote_ip", entry.RemoteIP)
	record.Set("method", entry.Method)
	record.Set("path", entry.Path)
	record.Set("status", entry.Status)
	record.Set("error_kind", entry.ErrorKind)
	record.Set("direction", entry.Direction)
	record.Set("bytes_transferred", entry.BytesTransferred)
	record.Set("duration_ms", entry.DurationMS)
	record.Set("upstream_status", entry.UpstreamStatus)
	record.Set("response_committed", entry.ResponseCommitted)
	return s.app.Save(record)
}

func (s *Store) CheckPathPolicy(ctx context.Context, method, relativePath string) (gateway.PathPolicyDecision, error) {
	policies, err := s.enabledPathPolicies()
	if err != nil {
		return gateway.PathPolicyDecision{}, err
	}
	return gateway.DecidePathPolicy(policies, method, relativePath), nil
}

func (s *Store) RecordPlaybackEvent(ctx context.Context, event gateway.PlaybackEvent) error {
	collection, err := s.app.FindCollectionByNameOrId("playback_events")
	if err != nil {
		return err
	}
	record := core.NewRecord(collection)
	record.Set("gateway_user", event.GatewayUserID)
	record.Set("synthetic_user_id", event.SyntheticUserID)
	record.Set("item_id", event.ItemID)
	record.Set("item_name", event.ItemName)
	record.Set("event", event.Event)
	record.Set("playback_position_ticks", event.PositionTicks)
	if event.Played != nil {
		record.Set("played", *event.Played)
	}
	if event.PlayedPercentage != nil {
		record.Set("played_percentage", *event.PlayedPercentage)
	}
	record.Set("remote_ip", event.RemoteIP)
	if !event.CreatedAt.IsZero() {
		record.Set("occurred_at", event.CreatedAt)
	} else {
		record.Set("occurred_at", time.Now().UTC())
	}
	return s.app.Save(record)
}

func (s *Store) FindPlaybackState(ctx context.Context, gatewayUserID, itemID string) (*gateway.PlaybackState, error) {
	records, err := s.app.FindRecordsByFilter(
		"user_item_data",
		"gateway_user = {:gatewayUserID} && item_id = {:itemID}",
		"",
		1,
		0,
		dbx.Params{"gatewayUserID": gatewayUserID, "itemID": itemID},
	)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, gateway.ErrNotFound
	}
	return playbackStateFromRecord(records[0]), nil
}

func (s *Store) ListPlaybackStatesByItemIDs(ctx context.Context, gatewayUserID string, itemIDs []string) (map[string]*gateway.PlaybackState, error) {
	if len(itemIDs) == 0 {
		return map[string]*gateway.PlaybackState{}, nil
	}
	states := map[string]*gateway.PlaybackState{}
	for start := 0; start < len(itemIDs); start += playbackStateItemIDBatchLimit {
		end := start + playbackStateItemIDBatchLimit
		if end > len(itemIDs) {
			end = len(itemIDs)
		}
		batchStates, err := s.listPlaybackStatesByItemIDBatch(ctx, gatewayUserID, itemIDs[start:end])
		if err != nil {
			return nil, err
		}
		for id, state := range batchStates {
			states[id] = state
		}
	}
	return states, nil
}

func (s *Store) listPlaybackStatesByItemIDBatch(ctx context.Context, gatewayUserID string, itemIDs []string) (map[string]*gateway.PlaybackState, error) {
	filterParts := make([]string, 0, len(itemIDs))
	params := dbx.Params{"gatewayUserID": gatewayUserID}
	for i, itemID := range itemIDs {
		if itemID == "" {
			continue
		}
		name := fmt.Sprintf("itemID%d", i)
		filterParts = append(filterParts, "item_id = {:"+name+"}")
		params[name] = itemID
	}
	if len(filterParts) == 0 {
		return map[string]*gateway.PlaybackState{}, nil
	}
	records, err := s.app.FindRecordsByFilter(
		"user_item_data",
		"gateway_user = {:gatewayUserID} && ("+strings.Join(filterParts, " || ")+")",
		"",
		0,
		0,
		params,
	)
	if err != nil {
		return nil, err
	}
	states := make(map[string]*gateway.PlaybackState, len(records))
	for _, record := range records {
		state := playbackStateFromRecord(record)
		if state.OrphanedAt != nil {
			continue
		}
		states[state.ItemID] = state
	}
	return states, nil
}

func (s *Store) ListPlaybackAggregates(ctx context.Context, gatewayUserID string, seriesIDs, seasonIDs []string) (gateway.PlaybackAggregates, error) {
	aggregates := gateway.PlaybackAggregates{Series: map[string]gateway.PlaybackAggregate{}, Seasons: map[string]gateway.PlaybackAggregate{}}
	seriesSet := stringSet(seriesIDs)
	seasonSet := stringSet(seasonIDs)
	if len(seriesSet) == 0 && len(seasonSet) == 0 {
		return aggregates, nil
	}
	records, err := s.app.FindRecordsByFilter(
		"user_item_data",
		"gateway_user = {:gatewayUserID}",
		"",
		0,
		0,
		dbx.Params{"gatewayUserID": gatewayUserID},
	)
	if err != nil {
		return aggregates, err
	}
	for _, record := range records {
		state := playbackStateFromRecord(record)
		if state.OrphanedAt != nil {
			continue
		}
		if seriesSet[state.SeriesID] {
			aggregates.Series[state.SeriesID] = addPlaybackAggregate(aggregates.Series[state.SeriesID], *state)
		}
		if seasonSet[state.SeasonID] {
			aggregates.Seasons[state.SeasonID] = addPlaybackAggregate(aggregates.Seasons[state.SeasonID], *state)
		}
	}
	return aggregates, nil
}

func (s *Store) ListItemChildCounts(ctx context.Context, backendAccountID string, itemIDs []string) (map[string]gateway.ItemChildCount, error) {
	counts := map[string]gateway.ItemChildCount{}
	if backendAccountID == "" || len(itemIDs) == 0 {
		return counts, nil
	}
	for start := 0; start < len(itemIDs); start += playbackStateItemIDBatchLimit {
		end := start + playbackStateItemIDBatchLimit
		if end > len(itemIDs) {
			end = len(itemIDs)
		}
		batch, err := s.listItemChildCountBatch(ctx, backendAccountID, itemIDs[start:end])
		if err != nil {
			return nil, err
		}
		for itemID, count := range batch {
			counts[itemID] = count
		}
	}
	return counts, nil
}

func (s *Store) listItemChildCountBatch(ctx context.Context, backendAccountID string, itemIDs []string) (map[string]gateway.ItemChildCount, error) {
	filterParts := make([]string, 0, len(itemIDs))
	params := dbx.Params{"backendAccountID": backendAccountID}
	for i, itemID := range itemIDs {
		if itemID == "" {
			continue
		}
		name := fmt.Sprintf("itemID%d", i)
		filterParts = append(filterParts, "item_id = {:"+name+"}")
		params[name] = itemID
	}
	if len(filterParts) == 0 {
		return map[string]gateway.ItemChildCount{}, nil
	}
	records, err := s.app.FindRecordsByFilter(
		"item_child_counts",
		"backend_account_id = {:backendAccountID} && ("+strings.Join(filterParts, " || ")+")",
		"",
		0,
		0,
		params,
	)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]gateway.ItemChildCount, len(records))
	for _, record := range records {
		count := itemChildCountFromRecord(record)
		counts[count.ItemID] = count
	}
	return counts, nil
}

func (s *Store) SaveItemChildCount(ctx context.Context, count gateway.ItemChildCount) error {
	if count.BackendAccountID == "" || count.ItemID == "" || count.ChildCount <= 0 {
		return nil
	}
	records, err := s.app.FindRecordsByFilter(
		"item_child_counts",
		"backend_account_id = {:backendAccountID} && item_id = {:itemID}",
		"",
		1,
		0,
		dbx.Params{"backendAccountID": count.BackendAccountID, "itemID": count.ItemID},
	)
	if err != nil {
		return err
	}
	var record *core.Record
	if len(records) > 0 {
		record = records[0]
	} else {
		collection, err := s.app.FindCollectionByNameOrId("item_child_counts")
		if err != nil {
			return err
		}
		record = core.NewRecord(collection)
		record.Set("backend_account_id", count.BackendAccountID)
		record.Set("item_id", count.ItemID)
	}
	record.Set("child_count", count.ChildCount)
	return s.app.Save(record)
}

func (s *Store) ListPlaybackStates(ctx context.Context, gatewayUserID string, filter gateway.PlaybackStateFilter) ([]gateway.PlaybackState, error) {
	records, err := s.app.FindRecordsByFilter(
		"user_item_data",
		"gateway_user = {:gatewayUserID}",
		"-updated",
		0,
		0,
		dbx.Params{"gatewayUserID": gatewayUserID},
	)
	if err != nil {
		return nil, err
	}
	states := make([]gateway.PlaybackState, 0, len(records))
	for _, record := range records {
		state := playbackStateFromRecord(record)
		if !filter.IncludeOrphaned && state.OrphanedAt != nil {
			continue
		}
		if filter.Played != nil && state.Played != *filter.Played {
			continue
		}
		if filter.Favorite != nil && state.IsFavorite != *filter.Favorite {
			continue
		}
		if filter.Resumable != nil {
			resumable := state.PlaybackPositionTicks > 0 && !state.Played
			if resumable != *filter.Resumable {
				continue
			}
		}
		if filter.SeriesID != "" && state.SeriesID != filter.SeriesID {
			continue
		}
		if filter.SeasonID != "" && state.SeasonID != filter.SeasonID {
			continue
		}
		states = append(states, *state)
	}
	return states, nil
}

func (s *Store) SavePlaybackState(ctx context.Context, state gateway.PlaybackState) error {
	records, err := s.app.FindRecordsByFilter(
		"user_item_data",
		"gateway_user = {:gatewayUserID} && item_id = {:itemID}",
		"",
		1,
		0,
		dbx.Params{"gatewayUserID": state.GatewayUserID, "itemID": state.ItemID},
	)
	if err != nil {
		return err
	}
	var record *core.Record
	if len(records) > 0 {
		record = records[0]
	} else {
		collection, err := s.app.FindCollectionByNameOrId("user_item_data")
		if err != nil {
			return err
		}
		record = core.NewRecord(collection)
		record.Set("gateway_user", state.GatewayUserID)
		record.Set("item_id", state.ItemID)
	}
	record.Set("synthetic_user_id", state.SyntheticUserID)
	record.Set("item_name", state.ItemName)
	record.Set("item_type", state.ItemType)
	record.Set("series_id", state.SeriesID)
	record.Set("series_name", state.SeriesName)
	record.Set("season_id", state.SeasonID)
	record.Set("index_number", state.IndexNumber)
	record.Set("parent_index_number", state.ParentIndexNumber)
	record.Set("run_time_ticks", state.RunTimeTicks)
	record.Set("played", state.Played)
	record.Set("playback_position_ticks", state.PlaybackPositionTicks)
	if state.PlayedPercentage != nil {
		record.Set("played_percentage", *state.PlayedPercentage)
		record.Set("played_percentage_set", true)
	} else {
		record.Set("played_percentage", nil)
		record.Set("played_percentage_set", false)
	}
	if state.LastPlayedDate != nil {
		record.Set("last_played_date", *state.LastPlayedDate)
	} else {
		record.Set("last_played_date", nil)
	}
	record.Set("play_count", state.PlayCount)
	record.Set("is_favorite", state.IsFavorite)
	if state.Likes != nil {
		record.Set("likes", *state.Likes)
		record.Set("likes_set", true)
	} else {
		record.Set("likes", false)
		record.Set("likes_set", false)
	}
	record.Set("fingerprint", state.Fingerprint)
	if state.OrphanedAt != nil {
		record.Set("orphaned_at", *state.OrphanedAt)
	} else {
		record.Set("orphaned_at", nil)
	}
	if state.LastSeenAt != nil {
		record.Set("last_seen_at", *state.LastSeenAt)
	} else {
		record.Set("last_seen_at", nil)
	}
	return s.app.Save(record)
}

func (s *Store) FindDisplayPreference(ctx context.Context, gatewayUserID, preferenceID, client string) (*gateway.DisplayPreference, error) {
	records, err := s.app.FindRecordsByFilter(
		"display_preferences",
		"gateway_user = {:gatewayUserID} && preference_id = {:preferenceID} && client = {:client}",
		"",
		1,
		0,
		dbx.Params{"gatewayUserID": gatewayUserID, "preferenceID": preferenceID, "client": client},
	)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, gateway.ErrNotFound
	}
	return displayPreferenceFromRecord(records[0]), nil
}

func (s *Store) SaveDisplayPreference(ctx context.Context, preference gateway.DisplayPreference) error {
	records, err := s.app.FindRecordsByFilter(
		"display_preferences",
		"gateway_user = {:gatewayUserID} && preference_id = {:preferenceID} && client = {:client}",
		"",
		1,
		0,
		dbx.Params{"gatewayUserID": preference.GatewayUserID, "preferenceID": preference.PreferenceID, "client": preference.Client},
	)
	if err != nil {
		return err
	}
	var record *core.Record
	if len(records) > 0 {
		record = records[0]
	} else {
		collection, err := s.app.FindCollectionByNameOrId("display_preferences")
		if err != nil {
			return err
		}
		record = core.NewRecord(collection)
		record.Set("gateway_user", preference.GatewayUserID)
		record.Set("preference_id", preference.PreferenceID)
		record.Set("client", preference.Client)
	}
	record.Set("synthetic_user_id", preference.SyntheticUserID)
	record.Set("payload_json", preference.PayloadJSON)
	return s.app.Save(record)
}

func stringSet(values []string) map[string]bool {
	set := map[string]bool{}
	for _, value := range values {
		if value != "" {
			set[value] = true
		}
	}
	return set
}

func addPlaybackAggregate(aggregate gateway.PlaybackAggregate, state gateway.PlaybackState) gateway.PlaybackAggregate {
	aggregate.KnownItemCount++
	if state.Played {
		aggregate.PlayedCount++
	}
	if state.LastPlayedDate != nil && (aggregate.LastPlayedDate == nil || state.LastPlayedDate.After(*aggregate.LastPlayedDate)) {
		t := *state.LastPlayedDate
		aggregate.LastPlayedDate = &t
	}
	activity := state.UpdatedAt
	if state.LastPlayedDate != nil && state.LastPlayedDate.After(activity) {
		activity = *state.LastPlayedDate
	}
	if !activity.IsZero() && (aggregate.LastActivityDate == nil || activity.After(*aggregate.LastActivityDate)) {
		t := activity
		aggregate.LastActivityDate = &t
	}
	return aggregate
}

func (s *Store) SaveSession(ctx context.Context, session *gateway.Session) error {
	collection, err := s.app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		return err
	}
	record := core.NewRecord(collection)
	record.Set("gateway_token_hash", session.GatewayTokenHash)
	record.Set("gateway_user", session.GatewayUserID)
	record.Set("gateway_username", session.GatewayUsername)
	record.Set("synthetic_user_id", session.SyntheticUserID)
	record.Set("backend_account", session.BackendAccountID)
	record.Set("client", session.Client)
	record.Set("device", session.Device)
	record.Set("device_id", session.DeviceID)
	record.Set("version", session.Version)
	record.Set("remote_ip", session.RemoteIP)
	record.Set("expires_at", session.ExpiresAt)
	return s.app.Save(record)
}

func (s *Store) SessionTokenExists(ctx context.Context, tokenHash string) (bool, error) {
	// Resolve the collection first so a missing/broken schema is an operational
	// error rather than a false "not found".
	if _, err := s.app.FindCollectionByNameOrId("gateway_sessions"); err != nil {
		return false, err
	}
	_, err := s.app.FindFirstRecordByData("gateway_sessions", "gateway_token_hash", tokenHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) FindSessionByTokenHash(ctx context.Context, tokenHash string) (*gateway.Session, error) {
	record, err := s.app.FindFirstRecordByData("gateway_sessions", "gateway_token_hash", tokenHash)
	if err != nil {
		return nil, gateway.ErrNotFound
	}
	createdAt := record.GetDateTime("created").Time()
	expiresAt := record.GetDateTime("expires_at").Time()
	var revokedAt *time.Time
	if !record.GetDateTime("revoked_at").IsZero() {
		t := record.GetDateTime("revoked_at").Time()
		revokedAt = &t
	}
	account, err := s.backendAccountByID(record.GetString("backend_account"))
	if err != nil {
		return nil, err
	}
	return &gateway.Session{
		GatewayTokenHash: record.GetString("gateway_token_hash"),
		GatewayUserID:    record.GetString("gateway_user"),
		GatewayUsername:  record.GetString("gateway_username"),
		SyntheticUserID:  record.GetString("synthetic_user_id"),
		BackendAccountID: record.GetString("backend_account"),
		BackendAccount:   *account,
		BackendServerID:  account.Server.BackendServerID,
		BackendBaseURL:   account.BaseURL,
		BackendUserID:    account.BackendUserID,
		BackendUsername:  account.Username,
		BackendToken:     account.BackendToken,
		BackendIdentity:  account.ClientIdentity.WithDefaults(),
		Client:           record.GetString("client"),
		Device:           record.GetString("device"),
		DeviceID:         record.GetString("device_id"),
		Version:          record.GetString("version"),
		RemoteIP:         record.GetString("remote_ip"),
		CreatedAt:        createdAt,
		ExpiresAt:        expiresAt,
		RevokedAt:        revokedAt,
	}, nil
}

func (s *Store) RevokeSession(ctx context.Context, tokenHash string) error {
	record, err := s.app.FindFirstRecordByData("gateway_sessions", "gateway_token_hash", tokenHash)
	if err != nil {
		return gateway.ErrNotFound
	}
	record.Set("revoked_at", time.Now().UTC())
	return s.app.Save(record)
}

func (s *Store) backendAccountByID(id string) (*gateway.BackendAccount, error) {
	if id == "" {
		return nil, gateway.ErrNotFound
	}
	record, err := s.app.FindRecordById("backend_accounts", id)
	if err != nil {
		return nil, gateway.ErrNotFound
	}
	return s.backendAccountFromRecord(record)
}

func (s *Store) backendAccountFromRecord(record *core.Record) (*gateway.BackendAccount, error) {
	if !record.GetBool("enabled") {
		return nil, gateway.ErrDisabled
	}
	serverID := record.GetString("server")
	server, err := s.app.FindRecordById("emby_servers", serverID)
	if err != nil {
		return nil, gateway.ErrNotFound
	}
	serverInfo := serverFromRecord(server)
	identity := serverInfo.ClientIdentity.WithDefaults()
	if strings.TrimSpace(identity.DeviceID) == "" {
		identity.DeviceID = gateway.StableBackendDeviceID(server.Id)
	}
	serverInfo.ClientIdentity = identity
	var tokenUpdatedAt *time.Time
	if !record.GetDateTime("token_updated_at").IsZero() {
		t := record.GetDateTime("token_updated_at").Time()
		tokenUpdatedAt = &t
	}
	var lastLoginAt *time.Time
	if !record.GetDateTime("last_login_at").IsZero() {
		t := record.GetDateTime("last_login_at").Time()
		lastLoginAt = &t
	}
	return &gateway.BackendAccount{
		ID:             record.Id,
		ServerID:       serverID,
		BaseURL:        serverInfo.BaseURL,
		Username:       record.GetString("backend_username"),
		Password:       record.GetString("backend_password"),
		Enabled:        record.GetBool("enabled") && serverInfo.Enabled,
		BackendUserID:  record.GetString("backend_user_id"),
		BackendToken:   record.GetString("backend_token"),
		TokenUpdatedAt: tokenUpdatedAt,
		LastLoginAt:    lastLoginAt,
		LastLoginError: record.GetString("last_login_error"),
		Server:         serverInfo,
		ClientIdentity: identity,
	}, nil
}

func serverFromRecord(record *core.Record) gateway.EmbyServer {
	identity := gateway.BackendClientIdentity{
		UserAgent: record.GetString("backend_user_agent"),
		Client:    record.GetString("backend_authorization_client"),
		Device:    record.GetString("backend_authorization_device"),
		DeviceID:  record.GetString("backend_authorization_device_id"),
		Version:   record.GetString("backend_authorization_version"),
	}.WithDefaults()
	if strings.TrimSpace(identity.DeviceID) == "" {
		identity.DeviceID = gateway.StableBackendDeviceID(record.Id)
	}
	var versionCheckedAt *time.Time
	if !record.GetDateTime("version_checked_at").IsZero() {
		t := record.GetDateTime("version_checked_at").Time()
		versionCheckedAt = &t
	}
	return gateway.EmbyServer{
		ID:               record.Id,
		Name:             record.GetString("name"),
		BaseURL:          record.GetString("base_url"),
		BackendServerID:  record.GetString("server_id"),
		ServerName:       record.GetString("server_name"),
		ServerVersion:    record.GetString("server_version"),
		VersionCheckedAt: versionCheckedAt,
		Enabled:          record.GetBool("enabled"),
		ClientIdentity:   identity,
	}
}

func (s *Store) enabledPathPolicies() ([]gateway.PathPolicy, error) {
	records, err := s.app.FindRecordsByFilter("path_policies", "enabled = true", "", 0, 0)
	if err != nil {
		return nil, err
	}
	policies := make([]gateway.PathPolicy, 0, len(records))
	for _, record := range records {
		policies = append(policies, gateway.PathPolicy{
			ID:       record.Id,
			Method:   record.GetString("method"),
			Path:     record.GetString("path"),
			Action:   record.GetString("action"),
			Reason:   record.GetString("reason"),
			Priority: record.GetInt("priority"),
			Enabled:  record.GetBool("enabled"),
		})
	}
	return policies, nil
}

func playbackStateFromRecord(record *core.Record) *gateway.PlaybackState {
	updatedAt := record.GetDateTime("updated").Time()
	var percentage *float64
	if record.GetBool("played_percentage_set") {
		v := record.GetFloat("played_percentage")
		percentage = &v
	}
	var lastPlayedDate *time.Time
	if !record.GetDateTime("last_played_date").IsZero() {
		t := record.GetDateTime("last_played_date").Time()
		lastPlayedDate = &t
	}
	var likes *bool
	if record.GetBool("likes_set") {
		v := record.GetBool("likes")
		likes = &v
	}
	var orphanedAt *time.Time
	if !record.GetDateTime("orphaned_at").IsZero() {
		t := record.GetDateTime("orphaned_at").Time()
		orphanedAt = &t
	}
	var lastSeenAt *time.Time
	if !record.GetDateTime("last_seen_at").IsZero() {
		t := record.GetDateTime("last_seen_at").Time()
		lastSeenAt = &t
	}
	return &gateway.PlaybackState{
		ID:                    record.Id,
		GatewayUserID:         record.GetString("gateway_user"),
		SyntheticUserID:       record.GetString("synthetic_user_id"),
		ItemID:                record.GetString("item_id"),
		ItemName:              record.GetString("item_name"),
		ItemType:              record.GetString("item_type"),
		SeriesID:              record.GetString("series_id"),
		SeriesName:            record.GetString("series_name"),
		SeasonID:              record.GetString("season_id"),
		IndexNumber:           record.GetInt("index_number"),
		ParentIndexNumber:     record.GetInt("parent_index_number"),
		RunTimeTicks:          int64(record.GetFloat("run_time_ticks")),
		PlaybackPositionTicks: int64(record.GetFloat("playback_position_ticks")),
		Played:                record.GetBool("played"),
		PlayedPercentage:      percentage,
		LastPlayedDate:        lastPlayedDate,
		PlayCount:             record.GetInt("play_count"),
		IsFavorite:            record.GetBool("is_favorite"),
		Likes:                 likes,
		Fingerprint:           record.GetString("fingerprint"),
		OrphanedAt:            orphanedAt,
		LastSeenAt:            lastSeenAt,
		UpdatedAt:             updatedAt,
	}
}

func displayPreferenceFromRecord(record *core.Record) *gateway.DisplayPreference {
	return &gateway.DisplayPreference{
		ID:              record.Id,
		GatewayUserID:   record.GetString("gateway_user"),
		SyntheticUserID: record.GetString("synthetic_user_id"),
		PreferenceID:    record.GetString("preference_id"),
		Client:          record.GetString("client"),
		PayloadJSON:     record.GetString("payload_json"),
		UpdatedAt:       record.GetDateTime("updated").Time(),
	}
}

func itemChildCountFromRecord(record *core.Record) gateway.ItemChildCount {
	return gateway.ItemChildCount{
		BackendAccountID: record.GetString("backend_account_id"),
		ItemID:           record.GetString("item_id"),
		ChildCount:       record.GetInt("child_count"),
		UpdatedAt:        record.GetDateTime("updated").Time(),
	}
}

func userFromRecord(record *core.Record) *gateway.GatewayUser {
	return &gateway.GatewayUser{
		ID:              record.Id,
		Username:        record.GetString("username"),
		SyntheticUserID: record.GetString("synthetic_user_id"),
		Enabled:         record.GetBool("enabled"),
	}
}
