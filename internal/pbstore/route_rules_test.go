package pbstore

import (
	"context"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func TestListRouteRulesReturnsOnlyEnabled(t *testing.T) {
	app := newTestApp(t)
	store := New(app)
	col, _ := app.FindCollectionByNameOrId("route_rules")
	for _, tc := range []struct{ path string; enabled bool }{
		{"/a", true}, {"/b", false}, {"/c", true},
	} {
		r := core.NewRecord(col)
		r.Set("path", tc.path)
		r.Set("target", "primary")
		r.Set("enabled", tc.enabled)
		if err := app.Save(r); err != nil { t.Fatal(err) }
	}
	rules, err := store.ListRouteRules(context.Background())
	if err != nil { t.Fatal(err) }
	if len(rules) != 2 {
		t.Fatalf("bool filter returned %d rules, want 2: %#v", len(rules), rules)
	}
	for _, r := range rules {
		if r.Path == "/b" { t.Fatalf("disabled rule returned: %#v", r) }
	}
	t.Log("pbstore bool filter OK, n =", len(rules))
}
