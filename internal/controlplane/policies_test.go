package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/pathpolicy"
)

func TestUpsertPolicyOptimisticConcurrency(t *testing.T) {
	app := newTestApp(t)

	created, err := UpsertPolicy(context.Background(), app, pathpolicy.Policy{
		Method:   "GET",
		Path:     "/Items/TestConcurrency",
		Action:   "deny",
		Reason:   "test",
		Priority: 50,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" || created.Updated.IsZero() {
		t.Fatalf("created policy missing id/updated: %#v", created)
	}

	// Stale updated token must conflict.
	stale := created.Updated.Add(-time.Hour)
	_, err = UpsertPolicy(context.Background(), app, pathpolicy.Policy{
		ID:       created.ID,
		Method:   "GET",
		Path:     "/Items/TestConcurrency",
		Action:   "allow",
		Reason:   "stale",
		Priority: 50,
		Enabled:  true,
		Updated:  stale,
	})
	if !errors.Is(err, ErrPolicyConflict) {
		t.Fatalf("stale update err = %v, want ErrPolicyConflict", err)
	}

	// Matching updated token succeeds.
	updated, err := UpsertPolicy(context.Background(), app, pathpolicy.Policy{
		ID:       created.ID,
		Method:   "GET",
		Path:     "/Items/TestConcurrency",
		Action:   "allow",
		Reason:   "ok",
		Priority: 60,
		Enabled:  true,
		Updated:  created.Updated,
	})
	if err != nil {
		t.Fatalf("fresh update: %v", err)
	}
	if updated.Action != "allow" || updated.Priority != 60 {
		t.Fatalf("updated = %#v", updated)
	}

	// Omitting Updated still updates (optional concurrency).
	_, err = UpsertPolicy(context.Background(), app, pathpolicy.Policy{
		ID:       created.ID,
		Method:   "GET",
		Path:     "/Items/TestConcurrency",
		Action:   "deny",
		Reason:   "no token",
		Priority: 70,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("update without token: %v", err)
	}
}
