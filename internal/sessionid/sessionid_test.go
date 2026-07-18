package sessionid

import (
	"strings"
	"testing"
)

func TestNewFormatAndValid(t *testing.T) {
	t.Parallel()

	id, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(id) != Length {
		t.Fatalf("len = %d, want %d (%q)", len(id), Length, id)
	}
	if !strings.HasPrefix(id, "session-") {
		t.Fatalf("missing prefix: %q", id)
	}
	if !Valid(id) {
		t.Fatalf("Valid rejected generated id %q", id)
	}
	// hex.EncodeToString is lowercase; ensure no uppercase slipped in.
	if id != strings.ToLower(id) {
		t.Fatalf("generated id is not lowercase: %q", id)
	}
}

func TestNewRepeatedUniqueness(t *testing.T) {
	t.Parallel()

	const n = 256
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id, err := New()
		if err != nil {
			t.Fatalf("New[%d]: %v", i, err)
		}
		if !Valid(id) {
			t.Fatalf("New[%d] invalid: %q", i, id)
		}
		if _, ok := seen[id]; ok {
			t.Fatalf("duplicate id at iteration %d: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestValidMalformed(t *testing.T) {
	t.Parallel()

	good, err := New()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		id   string
		want bool
	}{
		{"generated", good, true},
		{"empty", "", false},
		{"missing_prefix", "0123456789abcdef0123456789abcdef", false},
		{"wrong_prefix", "sessio-0123456789abcdef0123456789abcdef", false},
		{"uppercase_prefix", "SESSION-0123456789abcdef0123456789abcdef", false},
		{"uppercase_hex", "session-0123456789ABCDEF0123456789ABCDEF", false},
		{"mixed_case_hex", "session-0123456789abcdef0123456789ABCDEf", false},
		{"too_short", "session-0123456789abcdef0123456789abcde", false},
		{"too_long", "session-0123456789abcdef0123456789abcdef0", false},
		{"non_hex", "session-0123456789abcdef0123456789abcdeg", false},
		{"spaces", " session-0123456789abcdef0123456789abcdef", false},
		{"trailing_space", "session-0123456789abcdef0123456789abcdef ", false},
		{"token_shaped", "tok_" + strings.Repeat("a", 32), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Valid(tc.id); got != tc.want {
				t.Fatalf("Valid(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}
