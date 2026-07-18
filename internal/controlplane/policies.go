package controlplane

import (
	"context"
	"fmt"
	"strings"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/pathpolicy"
)

// InstallDefaultPolicies inserts missing default path policies without overwriting existing matches.
func InstallDefaultPolicies(app core.App) (created, preserved int, err error) {
	err = app.RunInTransaction(func(tx core.App) error {
		collection, err := tx.FindCollectionByNameOrId("path_policies")
		if err != nil {
			return err
		}
		records, err := tx.FindRecordsByFilter("path_policies", "", "", 0, 0)
		if err != nil {
			return err
		}
		existing := map[string]bool{}
		for _, r := range records {
			m, p := pathpolicy.NormalizedIdentity(r.GetString("method"), r.GetString("path"))
			existing[m+"\x00"+p] = true
		}
		for _, p := range pathpolicy.Defaults() {
			m, path := pathpolicy.NormalizedIdentity(p.Method, p.Path)
			key := m + "\x00" + path
			if existing[key] {
				preserved++
				continue
			}
			r := core.NewRecord(collection)
			r.Set("method", p.Method)
			r.Set("path", p.Path)
			r.Set("action", p.Action)
			r.Set("reason", p.Reason)
			r.Set("priority", p.Priority)
			r.Set("enabled", p.Enabled)
			if err := tx.Save(r); err != nil {
				return err
			}
			existing[key] = true
			created++
		}
		return nil
	})
	return
}

// ListPolicies returns all path policies.
func ListPolicies(ctx context.Context, app core.App) ([]pathpolicy.Policy, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	records, err := app.FindRecordsByFilter("path_policies", "", "priority", 0, 0)
	if err != nil {
		return nil, err
	}
	out := make([]pathpolicy.Policy, 0, len(records))
	for _, r := range records {
		out = append(out, policyFromRecord(r))
	}
	return out, nil
}

// UpsertPolicy creates or updates a path policy. Action must be allow/deny; method/path are normalized.
func UpsertPolicy(ctx context.Context, app core.App, p pathpolicy.Policy) (pathpolicy.Policy, error) {
	if err := ctx.Err(); err != nil {
		return pathpolicy.Policy{}, err
	}
	action := strings.ToLower(strings.TrimSpace(p.Action))
	if action != "allow" && action != "deny" {
		return pathpolicy.Policy{}, fmt.Errorf("action must be allow or deny")
	}
	method, path := pathpolicy.NormalizedIdentity(p.Method, p.Path)
	if path == "" {
		return pathpolicy.Policy{}, fmt.Errorf("path is required")
	}
	var out pathpolicy.Policy
	err := app.RunInTransaction(func(txApp core.App) error {
		collection, err := txApp.FindCollectionByNameOrId("path_policies")
		if err != nil {
			return err
		}
		var record *core.Record
		if id := strings.TrimSpace(p.ID); id != "" {
			record, err = txApp.FindRecordById("path_policies", id)
			if err != nil {
				return err
			}
		} else {
			record = core.NewRecord(collection)
		}
		record.Set("method", method)
		record.Set("path", path)
		record.Set("action", action)
		record.Set("reason", p.Reason)
		record.Set("priority", p.Priority)
		record.Set("enabled", p.Enabled)
		if err := txApp.Save(record); err != nil {
			return err
		}
		out = policyFromRecord(record)
		return nil
	})
	return out, err
}

// DeletePolicy deletes a path policy by id.
func DeletePolicy(ctx context.Context, app core.App, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("policy id is required")
	}
	record, err := app.FindRecordById("path_policies", id)
	if err != nil {
		return err
	}
	return app.Delete(record)
}

// PreviewPolicy evaluates method/path against stored policies.
func PreviewPolicy(ctx context.Context, app core.App, method, path string) (pathpolicy.Decision, error) {
	policies, err := ListPolicies(ctx, app)
	if err != nil {
		return pathpolicy.Decision{}, err
	}
	return pathpolicy.Decide(policies, method, path), nil
}

func policyFromRecord(r *core.Record) pathpolicy.Policy {
	p := pathpolicy.Policy{
		ID:       r.Id,
		Method:   r.GetString("method"),
		Path:     r.GetString("path"),
		Action:   r.GetString("action"),
		Reason:   r.GetString("reason"),
		Priority: r.GetInt("priority"),
		Enabled:  r.GetBool("enabled"),
	}
	if updated := r.GetDateTime("updated"); !updated.IsZero() {
		p.Updated = updated.Time().UTC()
	}
	return p
}
