package routepolicy

import (
	"strings"
	"testing"
)

func rule(id, method, path, transport, target string, priority int, enabled bool) Rule {
	return Rule{ID: id, Method: method, Path: path, Transport: transport, Target: target, Priority: priority, Enabled: enabled}
}

func TestSelectNoRulesFallsBackToEmpty(t *testing.T) {
	if got := Select(nil, Request{Method: "GET", Path: "/Items", Transport: TransportHTTP}); got != "" {
		t.Fatalf("Select(nil) = %q, want empty", got)
	}
}

func TestSelectMatchesExactPathAndMethod(t *testing.T) {
	rules := []Rule{rule("1", "GET", "/embywebsocket", TransportWebSocket, "ws", 100, true)}
	if got := Select(rules, Request{Method: "GET", Path: "/embywebsocket", Transport: TransportWebSocket}); got != "ws" {
		t.Fatalf("matched ws rule = %q", got)
	}
	if got := Select(rules, Request{Method: "POST", Path: "/embywebsocket", Transport: TransportWebSocket}); got != "" {
		t.Fatalf("wrong method must not match = %q", got)
	}
	if got := Select(rules, Request{Method: "GET", Path: "/embywebsocket", Transport: TransportHTTP}); got != "" {
		t.Fatalf("wrong transport must not match = %q", got)
	}
}

func TestTransportRulesGateWebSocketOnly(t *testing.T) {
	wsOnly := rule("1", "", "/socket", TransportWebSocket, "ws", 10, true)
	httpOnly := rule("2", "", "/socket", TransportHTTP, "http", 10, true)
	rules := []Rule{wsOnly, httpOnly}
	if got := Select(rules, Request{Method: "GET", Path: "/socket", Transport: TransportWebSocket}); got != "ws" {
		t.Fatalf("ws request = %q, want ws", got)
	}
	if got := Select(rules, Request{Method: "GET", Path: "/socket", Transport: TransportHTTP}); got != "http" {
		t.Fatalf("http request = %q, want http", got)
	}
}

func TestPriorityHigherWinsAcrossRules(t *testing.T) {
	rules := []Rule{
		rule("low", "GET", "/Items/*", "", "a", 10, true),
		rule("high", "GET", "/Items/*", "", "b", 100, true),
	}
	if got := Select(rules, Request{Method: "GET", Path: "/Items/1", Transport: TransportHTTP}); got != "b" {
		t.Fatalf("priority winner = %q, want b", got)
	}
}

func TestTransportSpecificityBeatsPriorityTie(t *testing.T) {
	generic := rule("g", "GET", "/socket", "", "a", 100, true)
	specific := rule("s", "GET", "/socket", TransportWebSocket, "ws", 100, true)
	rules := []Rule{generic, specific}
	// WS request: exact transport wins even at equal priority.
	if got := Select(rules, Request{Method: "GET", Path: "/socket", Transport: TransportWebSocket}); got != "ws" {
		t.Fatalf("ws specificity = %q, want ws", got)
	}
	// HTTP request: generic rule applies (specific is ws-only).
	if got := Select(rules, Request{Method: "GET", Path: "/socket", Transport: TransportHTTP}); got != "a" {
		t.Fatalf("http generic = %q, want a", got)
	}
}

func TestDisabledRulesIgnored(t *testing.T) {
	rules := []Rule{
		rule("on", "GET", "/x", "", "a", 10, true),
		rule("off", "GET", "/x", "", "b", 100, false),
	}
	if got := Select(rules, Request{Method: "GET", Path: "/x", Transport: TransportHTTP}); got != "a" {
		t.Fatalf("disabled must not win = %q", got)
	}
}

func TestWildcardMethodAndEmptyTransportMatchEverything(t *testing.T) {
	rules := []Rule{rule("w", "", "/*", "", "all", 5, true)}
	for _, req := range []Request{
		{Method: "GET", Path: "/A/B", Transport: TransportHTTP},
		{Method: "POST", Path: "/Users/AuthenticateByName", Transport: TransportHTTP},
		{Method: "GET", Path: "/embywebsocket", Transport: TransportWebSocket},
	} {
		if got := Select(rules, req); got != "all" {
			t.Fatalf("wildcard req %+v = %q", req, got)
		}
	}
}

func TestPatternParamAndPrefixMatching(t *testing.T) {
	param := rule("p", "GET", "/Items/{id}", "", "a", 1, true)
	prefix := rule("pfx", "GET", "/Users/*", "", "b", 1, true)
	rules := []Rule{param, prefix}
	cases := []struct {
		path string
		want string
	}{
		{"/Items/abc", "a"},
		{"/Users/def", "b"},
		{"/Items/abc/Extra", ""},
		{"/Users/def/Child", "b"},
	}
	for _, c := range cases {
		if got := Select(rules, Request{Method: "GET", Path: c.path, Transport: TransportHTTP}); got != c.want {
			t.Fatalf("path %q = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestDeterministicTieBreak(t *testing.T) {
	rules := []Rule{
		rule("b", "GET", "/x", TransportHTTP, "a", 1, true),
		rule("a", "GET", "/x", TransportHTTP, "b", 1, true),
	}
	want := "a" // same priority/specificity → lexicographic by ID (a < b) picks rule a → target a? target of rule a is b.
	// rule("a",...) target "b"; rule("b",...) target "a". Lexicographic ID a wins → target "b".
	if got := Select(rules, Request{Method: "GET", Path: "/x", Transport: TransportHTTP}); got != strings.ToLower(got) {
		t.Fatalf("deterministic tie sanity failed")
	}
	_ = want
	first := Select(rules, Request{Method: "GET", Path: "/x", Transport: TransportHTTP})
	for i := 0; i < 50; i++ {
		if got := Select(rules, Request{Method: "GET", Path: "/x", Transport: TransportHTTP}); got != first {
			t.Fatalf("nondeterministic selection: %q vs %q", got, first)
		}
	}
	if first != "b" {
		t.Fatalf("lexicographic id tie = %q, want b", first)
	}
}

func TestValidRejectsBadTransportAndEmptyTargetPath(t *testing.T) {
	bad := []Rule{
		{Path: "/x", Transport: "ftp", Target: "a"},
		{Path: "/x", Transport: "", Target: ""},
		{Path: "", Transport: "", Target: "a"},
	}
	for _, r := range bad {
		if err := r.Valid(); err == nil {
			t.Fatalf("Valid(%+v) accepted", r)
		}
	}
	if err := (Rule{Path: "/x", Transport: "", Target: "a"}).Valid(); err != nil {
		t.Fatalf("valid rule rejected: %v", err)
	}
}

func TestNormalizedIdentity(t *testing.T) {
	method, path, transport := NormalizedIdentity(Rule{Method: "get", Path: "/items/ ", Transport: "WebSocket"})
	if method != "GET" || path != "/items" || transport != "websocket" {
		t.Fatalf("NormalizedIdentity = %q/%q/%q", method, path, transport)
	}
}

func TestSelectRuleReturnsWinningRule(t *testing.T) {
	rules := []Rule{rule("x", "GET", "/only", TransportWebSocket, "ws", 5, true)}
	got, ok := SelectRule(rules, Request{Method: "GET", Path: "/only", Transport: TransportWebSocket})
	if !ok || got.ID != "x" {
		t.Fatalf("SelectRule = %+v ok=%v", got, ok)
	}
	if _, ok := SelectRule(rules, Request{Method: "GET", Path: "/other", Transport: TransportWebSocket}); ok {
		t.Fatalf("SelectRule matched unrelated path")
	}
}

func TestTransportNormalization(t *testing.T) {
	if normalizeTransport("") != TransportAny || normalizeTransport("HTTP") != TransportHTTP || normalizeTransport("WebSocket") != TransportWebSocket {
		t.Fatalf("normalizeTransport broken")
	}
	if normalizeTransport("bogus") != TransportAny {
		t.Fatalf("unknown transport must normalize to any")
	}
}
