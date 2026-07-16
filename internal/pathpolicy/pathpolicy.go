// Package pathpolicy provides path-policy matching and the built-in deny catalog.
package pathpolicy

import (
	"sort"
	"strings"
)

type Policy struct {
	ID       string
	Method   string
	Path     string
	Action   string
	Reason   string
	Priority int
	Enabled  bool
}

func (p Policy) Deny() bool { return strings.EqualFold(p.Action, "deny") }

type Decision struct {
	Allowed  bool
	Action   string
	PolicyID string
	Reason   string
}

func Decide(policies []Policy, method, path string) Decision {
	p, ok := FirstMatch(policies, method, path)
	if !ok {
		return Decision{Allowed: true, Action: "allow"}
	}
	if p.Deny() {
		return Decision{Action: "deny", PolicyID: p.ID, Reason: p.Reason}
	}
	return Decision{Allowed: true, Action: "allow", PolicyID: p.ID, Reason: p.Reason}
}

func FirstMatch(policies []Policy, method, path string) (Policy, bool) {
	matched := make([]Policy, 0, len(policies))
	for _, p := range policies {
		if p.Enabled && methodMatches(p.Method, method) && MatchPath(p.Path, path) {
			matched = append(matched, p)
		}
	}
	if len(matched) == 0 {
		return Policy{}, false
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Action != matched[j].Action {
			return strings.EqualFold(matched[i].Action, "deny")
		}
		if matched[i].Priority != matched[j].Priority {
			return matched[i].Priority > matched[j].Priority
		}
		if matched[i].Method != matched[j].Method {
			return matched[i].Method < matched[j].Method
		}
		if matched[i].Path != matched[j].Path {
			return matched[i].Path < matched[j].Path
		}
		return matched[i].ID < matched[j].ID
	})
	return matched[0], true
}

func methodMatches(policy, request string) bool {
	policy = strings.TrimSpace(policy)
	return policy == "" || policy == "*" || strings.EqualFold(policy, request)
}

// MatchPath retains legacy exact and terminal-star behavior, while parameters
// match exactly one decoded path segment.
func MatchPath(pattern, path string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(strings.ToLower(path), strings.ToLower(strings.TrimSuffix(pattern, "*")))
	}
	pattern, path = normalizePath(pattern), normalizePath(path)
	ps, rs := strings.Split(pattern, "/"), strings.Split(path, "/")
	if len(ps) != len(rs) {
		return false
	}
	for i := range ps {
		if parameter(ps[i]) {
			if rs[i] == "" {
				return false
			}
			continue
		}
		if !strings.EqualFold(ps[i], rs[i]) {
			return false
		}
	}
	return true
}

func parameter(s string) bool {
	if len(s) < 3 || s[0] != '{' || s[len(s)-1] != '}' {
		return false
	}
	s = s[1 : len(s)-1]
	for i := range s {
		c := s[i]
		if i == 0 {
			if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
				return false
			}
		} else if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func NormalizedIdentity(method, path string) (string, string) {
	return strings.ToUpper(strings.TrimSpace(method)), strings.ToLower(normalizePath(path))
}

func normalizePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || path == "/" {
		return path
	}
	return strings.TrimRight(path, "/")
}

func Defaults() []Policy {
	var out []Policy
	add := func(priority int, reason string, method string, paths ...string) {
		for _, path := range paths {
			out = append(out, Policy{Method: method, Path: path, Action: "deny", Enabled: true, Priority: priority, Reason: reason})
		}
	}
	add(1000, "account recovery", "POST", "/Users/ForgotPassword", "/Users/ForgotPassword/Pin", "/Users/{id}/Password")
	add(950, "user administration", "POST", "/Users/New", "/Users/{id}/Delete", "/Users/{id}/Policy", "/Users/{id}/Configuration", "/Users/{id}/Configuration/Partial", "/Users/{userId}/TypedSettings/{key}")
	add(950, "user administration", "DELETE", "/Users/{id}")
	add(900, "system mutation", "POST", "/System/Restart", "/System/Shutdown", "/System/Configuration", "/System/Configuration/Partial", "/System/Configuration/{key}")
	add(900, "sensitive system read", "GET", "/System/Configuration", "/System/Configuration/{key}", "/System/Logs/Query", "/System/Logs/{name}", "/System/Logs/{name}/Lines")
	add(850, "scheduled task mutation", "POST", "/ScheduledTasks/{id}/Triggers", "/ScheduledTasks/Running/{id}", "/ScheduledTasks/Running/{id}/Delete")
	add(850, "scheduled task mutation", "DELETE", "/ScheduledTasks/Running/{id}")
	add(850, "plugin or package administration", "POST", "/Plugins/{id}/Configuration", "/Plugins/{id}/Delete", "/Packages/Installed/{name}", "/Packages/Installing/{id}/Delete")
	add(850, "plugin or package administration", "DELETE", "/Plugins/{id}", "/Packages/Installing/{id}")
	add(850, "plugin or package administration", "GET", "/Plugins/{id}/Configuration")
	add(800, "destructive or library operation", "DELETE", "/Items", "/Items/{id}")
	add(800, "destructive or library operation", "POST", "/Items/Delete", "/Items/{id}/Delete", "/Library/Refresh", "/Library/Media/Updated", "/Library/Movies/Added", "/Library/Movies/Updated", "/Library/Series/Added", "/Library/Series/Updated")
	add(800, "destructive or library operation", "GET", "/Library/PhysicalPaths")
	add(750, "device administration", "DELETE", "/Devices")
	add(750, "device administration", "POST", "/Devices/Delete", "/Devices/Options", "/Devices/CameraUploads")
	return out
}
