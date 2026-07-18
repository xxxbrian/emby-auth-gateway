package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/sessionid"
)

func TestMemoryStoreCreateSessionDefaultsAndDuplicates(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	created, err := store.CreateSession(context.Background(), Session{
		GatewayTokenHash: "hash-1",
		GatewayUserID:    "user-1",
		GatewayUsername:  "alice",
		SyntheticUserID:  "syn-1",
		CreatedAt:        now,
		ExpiresAt:        now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !sessionid.Valid(created.PublicID) {
		t.Fatalf("PublicID invalid: %q", created.PublicID)
	}
	if created.Capabilities.RawJSON != "{}" {
		t.Fatalf("RawJSON = %q, want {}", created.Capabilities.RawJSON)
	}
	if !created.LastActivityAt.Equal(now) {
		t.Fatalf("LastActivityAt = %v, want %v", created.LastActivityAt, now)
	}

	// Returned value is a copy; mutating it must not affect store.
	created.PublicID = "mutated"
	found, err := store.FindSessionByTokenHash(context.Background(), "hash-1")
	if err != nil {
		t.Fatalf("FindSessionByTokenHash: %v", err)
	}
	if found.PublicID == "mutated" || !sessionid.Valid(found.PublicID) {
		t.Fatalf("store mutated via returned copy: %#v", found)
	}
	stableID := found.PublicID

	if _, err := store.CreateSession(context.Background(), Session{
		GatewayTokenHash: "hash-1",
		GatewayUserID:    "user-1",
		SyntheticUserID:  "syn-1",
		ExpiresAt:        now.Add(time.Hour),
	}); err == nil {
		t.Fatal("duplicate token hash: want error")
	}
	if _, err := store.CreateSession(context.Background(), Session{
		GatewayTokenHash: "hash-2",
		GatewayUserID:    "user-1",
		SyntheticUserID:  "syn-1",
		PublicID:         stableID,
		ExpiresAt:        now.Add(time.Hour),
	}); err == nil {
		t.Fatal("duplicate public id: want error")
	}
}

func TestMemoryStoreFindRepairsMissingProfileFields(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	createdAt := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	store.Sessions["legacy"] = &Session{
		GatewayTokenHash: "legacy",
		GatewayUserID:    "user-1",
		SyntheticUserID:  "syn-1",
		CreatedAt:        createdAt,
		ExpiresAt:        createdAt.Add(2 * time.Hour),
	}

	first, err := store.FindSessionByTokenHash(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("first find: %v", err)
	}
	if !sessionid.Valid(first.PublicID) {
		t.Fatalf("repaired PublicID invalid: %q", first.PublicID)
	}
	if first.Capabilities.RawJSON != "{}" {
		t.Fatalf("repaired RawJSON = %q", first.Capabilities.RawJSON)
	}
	if !first.LastActivityAt.Equal(createdAt) {
		t.Fatalf("repaired LastActivityAt = %v, want %v", first.LastActivityAt, createdAt)
	}

	second, err := store.FindSessionByTokenHash(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("second find: %v", err)
	}
	if second.PublicID != first.PublicID {
		t.Fatalf("repair not stable: %q vs %q", first.PublicID, second.PublicID)
	}
}

func TestMemoryStoreCapabilitiesTouchAndList(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	a, err := store.CreateSession(context.Background(), Session{
		GatewayTokenHash: "a",
		GatewayUserID:    "user-1",
		SyntheticUserID:  "syn-1",
		CreatedAt:        base,
		ExpiresAt:        base.Add(2 * time.Hour),
		LastActivityAt:   base,
	})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := store.CreateSession(context.Background(), Session{
		GatewayTokenHash: "b",
		GatewayUserID:    "user-1",
		SyntheticUserID:  "syn-1",
		CreatedAt:        base,
		ExpiresAt:        base.Add(2 * time.Hour),
		LastActivityAt:   base.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	// Same activity as b but lexicographically smaller PublicID for sort tie-break.
	cPublic := "session-" + strings.Repeat("0", 32)
	if b.PublicID < cPublic {
		cPublic = "session-" + strings.Repeat("f", 32)
	}
	c, err := store.CreateSession(context.Background(), Session{
		GatewayTokenHash: "c",
		GatewayUserID:    "user-1",
		SyntheticUserID:  "syn-1",
		PublicID:         cPublic,
		CreatedAt:        base,
		ExpiresAt:        base.Add(2 * time.Hour),
		LastActivityAt:   b.LastActivityAt,
	})
	if err != nil {
		t.Fatalf("create c: %v", err)
	}
	if _, err := store.CreateSession(context.Background(), Session{
		GatewayTokenHash: "other",
		GatewayUserID:    "user-2",
		SyntheticUserID:  "syn-2",
		CreatedAt:        base,
		ExpiresAt:        base.Add(2 * time.Hour),
	}); err != nil {
		t.Fatalf("create other: %v", err)
	}
	revokedAt := base.Add(30 * time.Minute)
	if _, err := store.CreateSession(context.Background(), Session{
		GatewayTokenHash: "revoked",
		GatewayUserID:    "user-1",
		SyntheticUserID:  "syn-1",
		CreatedAt:        base,
		ExpiresAt:        base.Add(2 * time.Hour),
		RevokedAt:        &revokedAt,
	}); err != nil {
		t.Fatalf("create revoked: %v", err)
	}
	if _, err := store.CreateSession(context.Background(), Session{
		GatewayTokenHash: "expired",
		GatewayUserID:    "user-1",
		SyntheticUserID:  "syn-1",
		CreatedAt:        base.Add(-3 * time.Hour),
		ExpiresAt:        base.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create expired: %v", err)
	}

	raw := `{"PlayableMediaTypes":["Video"],"SupportedCommands":["Play"],"SupportsMediaControl":true,"SupportsSync":false,"DeviceProfile":{"Name":"x"}}`
	updated, err := store.UpdateSessionCapabilities(context.Background(), "a", SessionCapabilities{RawJSON: raw}, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("UpdateSessionCapabilities: %v", err)
	}
	// Parse re-emits deterministic sorted-key canonical JSON.
	wantCanonical, err := ParseSessionCapabilities(raw)
	if err != nil {
		t.Fatalf("canonical parse: %v", err)
	}
	if updated.Capabilities.RawJSON != wantCanonical.RawJSON {
		t.Fatalf("capabilities raw = %q, want %q", updated.Capabilities.RawJSON, wantCanonical.RawJSON)
	}
	if len(updated.Capabilities.PlayableMediaTypes) != 1 || updated.Capabilities.PlayableMediaTypes[0] != "Video" {
		t.Fatalf("PlayableMediaTypes = %#v", updated.Capabilities.PlayableMediaTypes)
	}
	if !updated.Capabilities.SupportsMediaControl || updated.Capabilities.SupportsSync {
		t.Fatalf("bool fields = control=%v sync=%v", updated.Capabilities.SupportsMediaControl, updated.Capabilities.SupportsSync)
	}
	if !updated.LastActivityAt.Equal(base.Add(2 * time.Minute)) {
		t.Fatalf("activity after caps = %v", updated.LastActivityAt)
	}

	// Touch coalescing: within 30s should not change.
	changed, err := store.TouchSessionActivity(context.Background(), "a", base.Add(2*time.Minute+10*time.Second), 30*time.Second)
	if err != nil || changed {
		t.Fatalf("touch within interval = (%v, %v), want (false, nil)", changed, err)
	}
	changed, err = store.TouchSessionActivity(context.Background(), "a", base.Add(2*time.Minute+31*time.Second), 30*time.Second)
	if err != nil || !changed {
		t.Fatalf("touch after interval = (%v, %v), want (true, nil)", changed, err)
	}

	list, err := store.ListActiveSessions(context.Background(), "user-1", base.Add(time.Minute))
	if err != nil {
		t.Fatalf("ListActiveSessions: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("active count = %d, want 3 (%#v)", len(list), list)
	}
	// a has latest activity after touch; then b/c by activity then PublicID.
	if list[0].GatewayTokenHash != "a" {
		t.Fatalf("first session = %q, want a", list[0].GatewayTokenHash)
	}
	// b and c share activity before a's updates... wait, list uses current state.
	// a activity is base+2m+31s; b and c are base+1m. So order: a, then b/c by PublicID.
	secondID, thirdID := list[1].PublicID, list[2].PublicID
	if secondID > thirdID {
		t.Fatalf("tie-break sort failed: %q then %q", secondID, thirdID)
	}
	_ = a
	_ = c

	if _, err := store.UpdateSessionCapabilities(context.Background(), "missing", SessionCapabilities{RawJSON: "{}"}, base); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing caps update err = %v", err)
	}
	if _, err := store.TouchSessionActivity(context.Background(), "missing", base, time.Second); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing touch err = %v", err)
	}
	if err := store.RevokeSession(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing revoke err = %v", err)
	}
}

func TestMemoryStoreListFailsOnMalformedCapabilities(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Now().UTC()
	store.Sessions["bad"] = &Session{
		GatewayTokenHash: "bad",
		GatewayUserID:    "user-1",
		SyntheticUserID:  "syn-1",
		PublicID:         "session-" + strings.Repeat("a", 32),
		CreatedAt:        now,
		ExpiresAt:        now.Add(time.Hour),
		LastActivityAt:   now,
		Capabilities:     SessionCapabilities{RawJSON: "{not-json"},
	}
	if _, err := store.ListActiveSessions(context.Background(), "user-1", now); err == nil {
		t.Fatal("list with malformed capabilities: want error")
	}
}

func TestMemoryStoreRepairParityWithPersistedProfile(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	publicID := "session-" + strings.Repeat("b", 32)

	// Profile hole (PublicID empty): synthesize PublicID, default caps, activity from CreatedAt.
	t.Run("missing_profile_hole", func(t *testing.T) {
		store := NewMemoryStore()
		createdAt := now.Add(-time.Hour)
		store.Sessions["hole"] = &Session{
			GatewayTokenHash: "hole",
			GatewayUserID:    "user-1",
			SyntheticUserID:  "syn-1",
			CreatedAt:        createdAt,
			ExpiresAt:        now.Add(time.Hour),
		}
		found, err := store.FindSessionByTokenHash(context.Background(), "hole")
		if err != nil {
			t.Fatalf("find hole: %v", err)
		}
		if !sessionid.Valid(found.PublicID) {
			t.Fatalf("PublicID = %q", found.PublicID)
		}
		if found.Capabilities.RawJSON != "{}" {
			t.Fatalf("RawJSON = %q", found.Capabilities.RawJSON)
		}
		if !found.LastActivityAt.Equal(createdAt) {
			t.Fatalf("activity = %v, want %v", found.LastActivityAt, createdAt)
		}
	})

	// PublicID present + zero activity => integrity error (no CreatedAt fallback).
	t.Run("zero_activity", func(t *testing.T) {
		store := NewMemoryStore()
		store.Sessions["zero"] = &Session{
			GatewayTokenHash: "zero",
			GatewayUserID:    "user-1",
			SyntheticUserID:  "syn-1",
			PublicID:         publicID,
			CreatedAt:        now,
			ExpiresAt:        now.Add(time.Hour),
			Capabilities:     SessionCapabilities{RawJSON: "{}"},
		}
		if _, err := store.FindSessionByTokenHash(context.Background(), "zero"); err == nil {
			t.Fatal("zero activity: want integrity error")
		}
	})

	// PublicID present + empty capabilities => integrity error.
	t.Run("empty_capabilities", func(t *testing.T) {
		store := NewMemoryStore()
		store.Sessions["empty"] = &Session{
			GatewayTokenHash: "empty",
			GatewayUserID:    "user-1",
			SyntheticUserID:  "syn-1",
			PublicID:         publicID,
			CreatedAt:        now,
			ExpiresAt:        now.Add(time.Hour),
			LastActivityAt:   now,
			Capabilities:     SessionCapabilities{RawJSON: ""},
		}
		if _, err := store.FindSessionByTokenHash(context.Background(), "empty"); err == nil {
			t.Fatal("empty capabilities: want integrity error")
		}
	})

	// PublicID present + invalid capabilities => integrity error.
	t.Run("invalid_capabilities", func(t *testing.T) {
		store := NewMemoryStore()
		store.Sessions["inv"] = &Session{
			GatewayTokenHash: "inv",
			GatewayUserID:    "user-1",
			SyntheticUserID:  "syn-1",
			PublicID:         publicID,
			CreatedAt:        now,
			ExpiresAt:        now.Add(time.Hour),
			LastActivityAt:   now,
			Capabilities:     SessionCapabilities{RawJSON: `null`},
		}
		if _, err := store.FindSessionByTokenHash(context.Background(), "inv"); err == nil {
			t.Fatal("invalid capabilities: want integrity error")
		}
	})
}

func TestParseSessionCapabilities(t *testing.T) {
	t.Parallel()
	caps, err := ParseSessionCapabilities(`{"PlayableMediaTypes":["Audio","Video"],"SupportedCommands":["DisplayMessage"],"SupportsMediaControl":true,"SupportsSync":true}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !caps.SupportsMediaControl || !caps.SupportsSync {
		t.Fatalf("bools = %#v", caps)
	}
	if len(caps.PlayableMediaTypes) != 2 || caps.PlayableMediaTypes[0] != "Audio" {
		t.Fatalf("media types = %#v", caps.PlayableMediaTypes)
	}
	if _, err := ParseSessionCapabilities(`{`); err == nil {
		t.Fatal("malformed: want error")
	}
	empty, err := ParseSessionCapabilities("")
	if err != nil || empty.RawJSON != "{}" {
		t.Fatalf("empty default = %#v, %v", empty, err)
	}

	// Explicit null must reject (never converted to {}).
	if _, err := ParseSessionCapabilities(`null`); err == nil {
		t.Fatal("null: want error")
	}
	// Top-level array rejected.
	if _, err := ParseSessionCapabilities(`[]`); err == nil {
		t.Fatal("array: want error")
	}
	// Trailing data rejected.
	if _, err := ParseSessionCapabilities(`{}{}`); err == nil {
		t.Fatal("trailing data: want error")
	}
	// Oversize rejected.
	if _, err := ParseSessionCapabilities(`{"Pad":"` + strings.Repeat("x", sessionCapabilitiesMaxBytes) + `"}`); err == nil {
		t.Fatal("oversize: want error")
	}
	// Array bounds.
	media := make([]string, maxPlayableMediaTypes+1)
	for i := range media {
		media[i] = "M" + strconv.Itoa(i)
	}
	body, _ := json.Marshal(map[string]any{"PlayableMediaTypes": media})
	if _, err := ParseSessionCapabilities(string(body)); err == nil {
		t.Fatal("too many media types: want error")
	}
	// Bad DeviceProfile shapes.
	for _, bad := range []string{`{"DeviceProfile":[]}`, `{"DeviceProfile":"x"}`, `{"DeviceProfile":1}`, `{"DeviceProfile":true}`} {
		if _, err := ParseSessionCapabilities(bad); err == nil {
			t.Fatalf("DeviceProfile %s: want error", bad)
		}
	}
	// DeviceProfile null and object accepted; large unknown integer preserved exactly.
	big := `{"DeviceProfile":null,"Huge":9007199254740993,"SupportsSync":false}`
	parsed, err := ParseSessionCapabilities(big)
	if err != nil {
		t.Fatalf("large int parse: %v", err)
	}
	if !strings.Contains(parsed.RawJSON, `9007199254740993`) {
		t.Fatalf("large integer precision lost: %q", parsed.RawJSON)
	}
	if !strings.Contains(parsed.RawJSON, `"DeviceProfile":null`) {
		t.Fatalf("DeviceProfile null lost: %q", parsed.RawJSON)
	}
	// Deterministic key order + recursive compact for nested unknown whitespace.
	a, err := ParseSessionCapabilities(`{"SupportsSync":true,"Custom": { "n": 9007199254740993 }}`)
	if err != nil {
		t.Fatalf("det a: %v", err)
	}
	b, err := ParseSessionCapabilities(`{"Custom":{"n":9007199254740993},"SupportsSync":true}`)
	if err != nil {
		t.Fatalf("det b: %v", err)
	}
	if a.RawJSON != b.RawJSON {
		t.Fatalf("canonical not deterministic: %q vs %q", a.RawJSON, b.RawJSON)
	}
	again, err := ParseSessionCapabilities(a.RawJSON)
	if err != nil || again.RawJSON != a.RawJSON {
		t.Fatalf("not idempotent: %#v %v", again, err)
	}
}
