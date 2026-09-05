// Route rule administration for the upstream routing table. Rules map a
// method/path/transport request to an endpoint key; the gateway evaluates
// them (through internal/routepolicy) when selecting an endpoint.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/routepolicy"
)

// ErrRuleConflict is returned when an optimistic concurrency token does not
// match the stored route rule.
var ErrRuleConflict = errors.New("route rule was modified; reload and retry")

// RouteRuleDTO is the API view of a route rule row.
type RouteRuleDTO struct {
	ID        string    `json:"id"`
	Method    string    `json:"method,omitempty"`
	Path      string    `json:"path"`
	Transport string    `json:"transport,omitempty"`
	Target    string    `json:"target"`
	Priority  int       `json:"priority"`
	Enabled   bool      `json:"enabled"`
	Reason    string    `json:"reason,omitempty"`
	Updated   time.Time `json:"updated,omitempty"`
}

// ListRouteRules returns all route rules ordered by priority.
func ListRouteRules(ctx context.Context, app core.App) ([]RouteRuleDTO, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	records, err := app.FindRecordsByFilter("route_rules", "", "-priority", 0, 0)
	if err != nil {
		return nil, err
	}
	out := make([]RouteRuleDTO, 0, len(records))
	for _, record := range records {
		out = append(out, routeRuleDTOFromRecord(record))
	}
	return out, nil
}

// UpsertRouteRule creates or updates a route rule. Transport must be empty,
// "http", or "websocket"; path uses pathpolicy syntax.
func UpsertRouteRule(ctx context.Context, app core.App, rule routepolicy.Rule) (RouteRuleDTO, error) {
	if err := ctx.Err(); err != nil {
		return RouteRuleDTO{}, err
	}
	rule.Transport = strings.ToLower(strings.TrimSpace(rule.Transport))
	if rule.Transport == routepolicy.TransportHTTP {
		rule.Transport = ""
	}
	if err := rule.Valid(); err != nil {
		return RouteRuleDTO{}, err
	}
	rule.Method = strings.TrimSpace(rule.Method)
	if rule.Method == "*" {
		rule.Method = ""
	}
	rule.Target = strings.TrimSpace(rule.Target)
	rule.Path = strings.TrimSpace(rule.Path)
	var out RouteRuleDTO
	err := app.RunInTransaction(func(txApp core.App) error {
		collection, err := txApp.FindCollectionByNameOrId("route_rules")
		if err != nil {
			return err
		}
		var record *core.Record
		if id := strings.TrimSpace(rule.ID); id != "" {
			record, err = txApp.FindRecordById("route_rules", id)
			if err != nil {
				return err
			}
			if !rule.Updated.IsZero() {
				stored := record.GetDateTime("updated").Time().UTC()
				if stored.UnixMilli() != rule.Updated.UTC().UnixMilli() {
					return ErrRuleConflict
				}
			}
		} else {
			record = core.NewRecord(collection)
		}
		record.Set("method", rule.Method)
		record.Set("path", rule.Path)
		record.Set("transport", rule.Transport)
		record.Set("target", rule.Target)
		record.Set("priority", rule.Priority)
		record.Set("enabled", rule.Enabled)
		record.Set("reason", rule.Reason)
		if err := txApp.Save(record); err != nil {
			return err
		}
		out = routeRuleDTOFromRecord(record)
		return nil
	})
	return out, err
}

// DeleteRouteRule removes a route rule by id.
func DeleteRouteRule(ctx context.Context, app core.App, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("route rule id is required")
	}
	record, err := app.FindRecordById("route_rules", id)
	if err != nil {
		return err
	}
	return app.Delete(record)
}

// EndpointByID returns the endpoint row by id (used by admin probe handlers).
func EndpointByID(ctx context.Context, app core.App, id string) (EndpointDTO, error) {
	if err := ctx.Err(); err != nil {
		return EndpointDTO{}, err
	}
	record, err := app.FindRecordById(UpstreamEndpoints, strings.TrimSpace(id))
	if err != nil {
		return EndpointDTO{}, err
	}
	return endpointDTOFromRecord(record), nil
}

func routeRuleDTOFromRecord(record *core.Record) RouteRuleDTO {
	dto := RouteRuleDTO{
		ID:        record.Id,
		Method:    record.GetString("method"),
		Path:      record.GetString("path"),
		Transport: record.GetString("transport"),
		Target:    record.GetString("target"),
		Priority:  record.GetInt("priority"),
		Enabled:   record.GetBool("enabled"),
		Reason:    record.GetString("reason"),
	}
	if t := record.GetDateTime("updated"); !t.IsZero() {
		dto.Updated = t.Time().UTC()
	}
	return dto
}
