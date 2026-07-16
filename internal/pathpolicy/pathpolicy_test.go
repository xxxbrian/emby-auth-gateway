package pathpolicy

import (
	"strconv"
	"strings"
	"testing"
)

func TestMatchPathParametersAndLegacyPatterns(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"/Users/{id}/Password", "/users/abc/Password", true},
		{"/Users/{id}/Password", "/Users//Password", false},
		{"/Users/{id}/Password", "/Users/a/b/Password", false},
		{"/Users/{1id}/Password", "/Users/abc/Password", false},
		{"/Users/{id}x", "/Users/abcx", false},
		{"/Items/*", "/items/a/b", true},
		{"/Items", "/items/", true},
		{"/Users/{id}/Password", "/Users/abc/Password///", true},
		{"/Items/*", "/items", false},
		{"/Items/*", "/items/", true},
	} {
		if got := MatchPath(tc.pattern, tc.path); got != tc.want {
			t.Errorf("MatchPath(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestPolicyOrderingAndMethods(t *testing.T) {
	policies := []Policy{{ID: "allow", Method: "POST", Path: "/x", Action: "allow", Enabled: true, Priority: 999}, {ID: "low", Method: "GET", Path: "/x", Action: "deny", Enabled: true, Priority: 1}, {ID: "high", Method: "GET", Path: "/x", Action: "deny", Enabled: true, Priority: 2}}
	if got := Decide(policies, "GET", "/x"); got.Allowed || got.PolicyID != "high" {
		t.Fatalf("decision = %#v", got)
	}
	if got := Decide(policies, "POST", "/x"); !got.Allowed || got.PolicyID != "allow" {
		t.Fatalf("method decision = %#v", got)
	}
	if got := Decide(append(policies, Policy{ID: "deny", Method: "POST", Path: "/x", Action: "deny", Enabled: true}), "POST", "/x"); got.Allowed || got.PolicyID != "deny" {
		t.Fatalf("deny precedence = %#v", got)
	}
	if got := Decide(Defaults(), "POST", "/Users/AuthenticateByName"); !got.Allowed {
		t.Fatalf("login unexpectedly denied: %#v", got)
	}
}

func TestNormalizedIdentityTrailingSlashes(t *testing.T) {
	if method, path := NormalizedIdentity(" post ", "/Users/x/Password///"); method != "POST" || path != "/users/x/password" {
		t.Fatalf("identity = %q %q", method, path)
	}
	if _, path := NormalizedIdentity("GET", "/"); path != "/" {
		t.Fatalf("root identity = %q", path)
	}
}

func TestDefaultsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Defaults() {
		method, path := NormalizedIdentity(p.Method, p.Path)
		key := method + "\x00" + path
		if seen[key] {
			t.Fatalf("duplicate %s", key)
		}
		seen[key] = true
		if !p.Enabled || p.Action != "deny" {
			t.Fatalf("invalid default %#v", p)
		}
	}
	if len(seen) != 46 {
		t.Fatalf("catalog count = %d, want 46", len(seen))
	}
}

func TestDefaultCatalogTuplesAndDecisions(t *testing.T) {
	want := []string{
		"POST|/Users/ForgotPassword|1000|account recovery", "POST|/Users/ForgotPassword/Pin|1000|account recovery", "POST|/Users/{id}/Password|1000|account recovery",
		"POST|/Users/New|950|user administration", "POST|/Users/{id}/Delete|950|user administration", "POST|/Users/{id}/Policy|950|user administration", "POST|/Users/{id}/Configuration|950|user administration", "POST|/Users/{id}/Configuration/Partial|950|user administration", "POST|/Users/{userId}/TypedSettings/{key}|950|user administration", "DELETE|/Users/{id}|950|user administration",
		"POST|/System/Restart|900|system mutation", "POST|/System/Shutdown|900|system mutation", "POST|/System/Configuration|900|system mutation", "POST|/System/Configuration/Partial|900|system mutation", "POST|/System/Configuration/{key}|900|system mutation",
		"GET|/System/Configuration|900|sensitive system read", "GET|/System/Configuration/{key}|900|sensitive system read", "GET|/System/Logs/Query|900|sensitive system read", "GET|/System/Logs/{name}|900|sensitive system read", "GET|/System/Logs/{name}/Lines|900|sensitive system read",
		"POST|/ScheduledTasks/{id}/Triggers|850|scheduled task mutation", "POST|/ScheduledTasks/Running/{id}|850|scheduled task mutation", "POST|/ScheduledTasks/Running/{id}/Delete|850|scheduled task mutation", "DELETE|/ScheduledTasks/Running/{id}|850|scheduled task mutation",
		"POST|/Plugins/{id}/Configuration|850|plugin or package administration", "POST|/Plugins/{id}/Delete|850|plugin or package administration", "POST|/Packages/Installed/{name}|850|plugin or package administration", "POST|/Packages/Installing/{id}/Delete|850|plugin or package administration", "DELETE|/Plugins/{id}|850|plugin or package administration", "DELETE|/Packages/Installing/{id}|850|plugin or package administration", "GET|/Plugins/{id}/Configuration|850|plugin or package administration",
		"DELETE|/Items|800|destructive or library operation", "DELETE|/Items/{id}|800|destructive or library operation", "POST|/Items/Delete|800|destructive or library operation", "POST|/Items/{id}/Delete|800|destructive or library operation", "POST|/Library/Refresh|800|destructive or library operation", "POST|/Library/Media/Updated|800|destructive or library operation", "POST|/Library/Movies/Added|800|destructive or library operation", "POST|/Library/Movies/Updated|800|destructive or library operation", "POST|/Library/Series/Added|800|destructive or library operation", "POST|/Library/Series/Updated|800|destructive or library operation", "GET|/Library/PhysicalPaths|800|destructive or library operation",
		"DELETE|/Devices|750|device administration", "POST|/Devices/Delete|750|device administration", "POST|/Devices/Options|750|device administration", "POST|/Devices/CameraUploads|750|device administration",
	}
	got := map[string]bool{}
	for _, p := range Defaults() {
		if p.Action != "deny" || !p.Enabled {
			t.Fatalf("default is not enabled deny: %#v", p)
		}
		got[p.Method+"|"+p.Path+"|"+itoa(p.Priority)+"|"+p.Reason] = true
		path := instantiate(p.Path)
		if decision := Decide(Defaults(), p.Method, path); decision.Allowed {
			t.Fatalf("default does not deny %s %s", p.Method, path)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("catalog size = %d, want %d", len(got), len(want))
	}
	for _, tuple := range want {
		if !got[tuple] {
			t.Errorf("missing tuple %q", tuple)
		}
	}
}

func instantiate(path string) string {
	return strings.NewReplacer("{id}", "x", "{userId}", "x", "{key}", "x", "{name}", "x").Replace(path)
}
func itoa(n int) string { return strconv.Itoa(n) }
