// Endpoint administration for the singleton upstream source. Endpoints are
// physical ingress URLs sharing the source's single credential/token; the
// control plane manages them independently of credentials (which belong to
// the source record and are edited through reconfigure).
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
)

// ErrEndpointInvalid is returned when an endpoint mutation violates topology
// rules (missing default, duplicate default, disabled default, unknown key).
var ErrEndpointInvalid = errors.New("invalid upstream endpoint configuration")

// EndpointUpsertInput describes an endpoint create/update.
type EndpointUpsertInput struct {
	ID        string // empty on create
	Key       string
	BaseURL   string
	Enabled   bool
	IsDefault bool
}

// EndpointDTO is the non-secret view of an endpoint row.
type EndpointDTO struct {
	ID                  string     `json:"id"`
	Key                 string     `json:"key"`
	BaseURL             string     `json:"base_url"`
	Enabled             bool       `json:"enabled"`
	IsDefault           bool       `json:"is_default"`
	WebSocketCapable    bool       `json:"websocket_capable"`
	WebSocketProbedAt   *time.Time `json:"websocket_probed_at,omitempty"`
	WebSocketProbeError string     `json:"websocket_probe_error,omitempty"`
	Updated             time.Time  `json:"updated,omitempty"`
}

// ListEndpoints returns all endpoint rows for the default source.
func ListEndpoints(ctx context.Context, app core.App) ([]EndpointDTO, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state, err := LoadUpstreamState(app)
	if err != nil {
		return nil, err
	}
	if state.Source == nil {
		return nil, nil
	}
	records, err := app.FindRecordsByFilter(UpstreamEndpoints, "source = {:source}", "created", 0, 0, map[string]any{"source": state.Source.Id})
	if err != nil {
		return nil, err
	}
	out := make([]EndpointDTO, 0, len(records))
	for _, record := range records {
		out = append(out, endpointDTOFromRecord(record))
	}
	return out, nil
}

// UpsertEndpoint creates or updates an endpoint row for the default source.
// Exactly one endpoint must remain the enabled default afterwards; the caller
// supplies that intent explicitly (IsDefault + Enabled), and the transaction
// clears competing defaults when this row becomes the default.
func UpsertEndpoint(ctx context.Context, app core.App, in EndpointUpsertInput) (EndpointDTO, error) {
	if err := ctx.Err(); err != nil {
		return EndpointDTO{}, err
	}
	key := strings.TrimSpace(in.Key)
	baseURL, err := NormalizeUpstreamURL(in.BaseURL)
	if err != nil {
		return EndpointDTO{}, err
	}
	if key == "" {
		return EndpointDTO{}, fmt.Errorf("%w: endpoint key is required", ErrEndpointInvalid)
	}
	if err := ctx.Err(); err != nil {
		return EndpointDTO{}, err
	}
	state, err := LoadUpstreamState(app)
	if err != nil {
		return EndpointDTO{}, err
	}
	if state.Source == nil {
		return EndpointDTO{}, fmt.Errorf("%w: upstream source is not configured", ErrEndpointInvalid)
	}
	var out EndpointDTO
	err = app.RunInTransaction(func(txApp core.App) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := LoadUpstreamStateForCreate(txApp)
		if err != nil {
			return err
		}
		if current.Source == nil {
			return fmt.Errorf("%w: upstream source is not configured", ErrEndpointInvalid)
		}
		var record *core.Record
		if in.ID != "" {
			record, err = txApp.FindRecordById(UpstreamEndpoints, in.ID)
			if err != nil {
				return err
			}
			if record.GetString("source") != current.Source.Id {
				return fmt.Errorf("%w: endpoint belongs to another source", ErrEndpointInvalid)
			}
		} else {
			collection, err := txApp.FindCollectionByNameOrId(UpstreamEndpoints)
			if err != nil {
				return err
			}
			record = core.NewRecord(collection)
			record.Set("source", current.Source.Id)
		}
		// Collision: same source+key or source+base_url must be unique.
		existing, err := txApp.FindRecordsByFilter(UpstreamEndpoints, "source = {:source} && (key = {:key} || base_url = {:url})", "", 0, 0, map[string]any{"source": current.Source.Id, "key": key, "url": baseURL})
		if err != nil {
			return err
		}
		for _, other := range existing {
			if in.ID != "" && other.Id == in.ID {
				continue
			}
			return fmt.Errorf("%w: another endpoint already uses key %q or URL %q", ErrEndpointInvalid, key, baseURL)
		}
		record.Set("key", key)
		record.Set("base_url", baseURL)
		record.Set("enabled", in.Enabled)
		// When this row becomes the default, clear competing defaults BEFORE
		// saving it (the partial unique index on is_default=1 forbids two rows
		// being default at the same instant).
		if in.IsDefault && !record.GetBool("is_default") {
			all, err := txApp.FindRecordsByFilter(UpstreamEndpoints, "source = {:source}", "", 0, 0, map[string]any{"source": current.Source.Id})
			if err != nil {
				return err
			}
			for _, other := range all {
				if other.Id != record.Id && other.GetBool("is_default") {
					other.Set("is_default", false)
					if err := txApp.Save(other); err != nil {
						return err
					}
				}
			}
		}
		record.Set("is_default", in.IsDefault)
		if err := txApp.Save(record); err != nil {
			return err
		}
		// A disabled default is invalid: when disabling the default, promote the
		// first enabled endpoint to default (fail-closed otherwise). Boolean
		// filters are unreliable in PocketBase filter strings, so evaluate the
		// full source endpoint set in Go.
		all, err := txApp.FindRecordsByFilter(UpstreamEndpoints, "source = {:source}", "created", 0, 0, map[string]any{"source": current.Source.Id})
		if err != nil {
			return err
		}
		hasEnabledDefault := false
		for _, ep := range all {
			if ep.GetBool("enabled") && ep.GetBool("is_default") {
				hasEnabledDefault = true
				break
			}
		}
		if !hasEnabledDefault {
			var promoted *core.Record
			for _, ep := range all {
				if ep.GetBool("enabled") {
					promoted = ep
					break
				}
			}
			if promoted == nil {
				return fmt.Errorf("%w: at least one enabled endpoint is required", ErrEndpointInvalid)
			}
			if !promoted.GetBool("is_default") {
				promoted.Set("is_default", true)
				if err := txApp.Save(promoted); err != nil {
					return err
				}
			}
		}
		out = endpointDTOFromRecord(record)
		return nil
	})
	return out, err
}

// DeleteEndpoint removes an endpoint row. The default endpoint cannot be
// deleted while other endpoints exist; deleting it when it is the only
// endpoint is refused (a source always needs one enabled default).
func DeleteEndpoint(ctx context.Context, app core.App, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("endpoint id is required")
	}
	state, err := LoadUpstreamState(app)
	if err != nil {
		return err
	}
	if state.Source == nil {
		return nil
	}
	return app.RunInTransaction(func(txApp core.App) error {
		record, err := txApp.FindRecordById(UpstreamEndpoints, id)
		if err != nil {
			return err
		}
		if record.GetString("source") != state.Source.Id {
			return fmt.Errorf("%w: endpoint belongs to another source", ErrEndpointInvalid)
		}
		if record.GetBool("is_default") {
			others, err := txApp.FindRecordsByFilter(UpstreamEndpoints, "source = {:source} && id != {:id}", "", 0, 1, map[string]any{"source": state.Source.Id, "id": id})
			if err != nil {
				return err
			}
			if len(others) > 0 {
				return fmt.Errorf("%w: set another default endpoint before deleting the default", ErrEndpointInvalid)
			}
			return fmt.Errorf("%w: the only endpoint cannot be deleted", ErrEndpointInvalid)
		}
		return txApp.Delete(record)
	})
}

// RecordWebSocketProbeResult persists a probe outcome on an endpoint.
func RecordWebSocketProbeResult(ctx context.Context, app core.App, endpointID string, capable bool, probeErr error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	record, err := app.FindRecordById(UpstreamEndpoints, strings.TrimSpace(endpointID))
	if err != nil {
		return err
	}
	record.Set("websocket_capable", capable)
	record.Set("websocket_probed_at", time.Now().UTC())
	if probeErr != nil {
		message := probeErr.Error()
		if len(message) > 512 {
			message = message[:512]
		}
		record.Set("websocket_probe_error", message)
	} else {
		record.Set("websocket_probe_error", "")
	}
	return app.Save(record)
}

// endpointRecordByID is used internally by tests/adminapi for targeted access.
func endpointRecordByID(ctx context.Context, app core.App, id string) (*core.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return app.FindRecordById(UpstreamEndpoints, strings.TrimSpace(id))
}

func endpointDTOFromRecord(record *core.Record) EndpointDTO {
	dto := EndpointDTO{
		ID:                  record.Id,
		Key:                 record.GetString("key"),
		BaseURL:             record.GetString("base_url"),
		Enabled:             record.GetBool("enabled"),
		IsDefault:           record.GetBool("is_default"),
		WebSocketCapable:    record.GetBool("websocket_capable"),
		WebSocketProbeError: record.GetString("websocket_probe_error"),
	}
	if t := record.GetDateTime("websocket_probed_at"); !t.IsZero() {
		tt := t.Time().UTC()
		dto.WebSocketProbedAt = &tt
	}
	if t := record.GetDateTime("updated"); !t.IsZero() {
		tt := t.Time().UTC()
		dto.Updated = tt
	}
	return dto
}

// validateEndpointTopologyAfterMutation is a defensive consistency check after
// any endpoint write (used by tests); it mirrors gateway.ValidateUpstreamEndpoints.
func validateEndpointTopologyAfterMutation(app core.App) error {
	state, err := LoadUpstreamState(app)
	if err != nil {
		return err
	}
	if state.Source == nil {
		return gateway.ErrUpstreamNotFound
	}
	endpoints := make(gateway.UpstreamEndpoints, 0, len(state.Endpoints))
	for _, record := range state.Endpoints {
		endpoint := gateway.UpstreamEndpoint{
			ID: record.Id, SourceID: record.GetString("source"), Key: record.GetString("key"),
			BaseURL: record.GetString("base_url"), Enabled: record.GetBool("enabled"),
			Default: record.GetBool("is_default"),
		}
		endpoints = append(endpoints, endpoint)
	}
	return gateway.ValidateUpstreamEndpoints(state.Source.Id, endpoints)
}
