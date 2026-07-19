package gateway

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMediaLeaseRegisterValidateAndRefresh(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	registry := NewMediaLeaseRegistry(func() time.Time { return now })

	if err := registry.Register(MediaLease{GatewayTokenHash: "owner", PlaySessionID: " play ", LiveStreamID: " live "}); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := registry.Validate("owner", "play", "live", now)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got.PlaySessionID != "play" || got.LiveStreamID != "live" || !got.ExpiresAt.Equal(now.Add(mediaLeaseTTL)) {
		t.Fatalf("validated lease has unexpected canonical identifiers or expiry")
	}

	now = now.Add(time.Hour)
	if err := registry.Register(MediaLease{GatewayTokenHash: "owner", PlaySessionID: "play", LiveStreamID: "live"}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got, err = registry.Validate("owner", "play", "live", now)
	if err != nil {
		t.Fatalf("validate refreshed lease: %v", err)
	}
	if !got.ExpiresAt.Equal(now.Add(mediaLeaseTTL)) {
		t.Fatal("idempotent registration did not refresh expiry")
	}
}

func TestMediaLeaseKindsAreSeparate(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	registry := NewMediaLeaseRegistry(func() time.Time { return now })
	if err := registry.Register(MediaLease{GatewayTokenHash: "play-owner", PlaySessionID: "same"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(MediaLease{GatewayTokenHash: "live-owner", LiveStreamID: "same"}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Validate("play-owner", "same", "", now); err != nil {
		t.Fatalf("play lease: %v", err)
	}
	if _, err := registry.Validate("live-owner", "", "same", now); err != nil {
		t.Fatalf("live lease: %v", err)
	}
}

func TestMediaLeaseForeignUnknownAndExpiredAreNotFound(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	registry := NewMediaLeaseRegistry(func() time.Time { return now })
	if err := registry.Register(MediaLease{GatewayTokenHash: "owner", PlaySessionID: "play"}); err != nil {
		t.Fatal(err)
	}

	checks := []struct {
		owner string
		id    PlaySessionID
		at    time.Time
	}{
		{owner: "foreign", id: "play", at: now},
		{owner: "owner", id: "unknown", at: now},
		{owner: "owner", id: "play", at: now.Add(mediaLeaseTTL)},
	}
	for _, check := range checks {
		if _, err := registry.Validate(check.owner, check.id, "", check.at); !errors.Is(err, ErrNotFound) {
			t.Fatalf("validate rejection = %v, want ErrNotFound", err)
		}
	}
	if got := registry.Sweep(now.Add(mediaLeaseTTL)); got != 0 {
		t.Fatalf("expired validation did not remove lease; sweep removed %d", got)
	}
}

func TestMediaLeaseRejectsUnboundedOrEmptyRegistration(t *testing.T) {
	registry := NewMediaLeaseRegistry(nil)
	checks := []MediaLease{
		{GatewayTokenHash: "owner"},
		{GatewayTokenHash: " owner", PlaySessionID: "play"},
		{GatewayTokenHash: strings.Repeat("o", mediaLeaseOwnerMaxBytes+1), PlaySessionID: "play"},
		{GatewayTokenHash: "owner", PlaySessionID: PlaySessionID(strings.Repeat("p", mediaLeaseIdentifierMaxBytes+1))},
		{GatewayTokenHash: "owner", LiveStreamID: LiveStreamID(strings.Repeat("l", mediaLeaseIdentifierMaxBytes+1))},
	}
	for _, lease := range checks {
		if err := registry.Register(lease); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("invalid registration error = %v, want ErrBadRequest", err)
		}
	}
	if _, err := registry.Validate("owner", PlaySessionID(strings.Repeat("p", mediaLeaseIdentifierMaxBytes+1)), "", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalid validation error = %v, want ErrNotFound", err)
	}
}

func TestMediaLeaseCapacityLimits(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	registry := NewMediaLeaseRegistry(func() time.Time { return now })
	for i := 0; i < mediaLeaseRegistryMaxPerToken; i++ {
		if err := registry.Register(MediaLease{GatewayTokenHash: "owner", PlaySessionID: PlaySessionID(fmt.Sprintf("p-%d", i))}); err != nil {
			t.Fatalf("fill per-token capacity at %d: %v", i, err)
		}
	}
	if err := registry.Register(MediaLease{GatewayTokenHash: "owner", PlaySessionID: "overflow"}); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("per-token overflow = %v, want ErrStoreUnavailable", err)
	}

	registry = NewMediaLeaseRegistry(func() time.Time { return now })
	for i := 0; i < mediaLeaseRegistryMaxGlobal; i++ {
		owner := fmt.Sprintf("owner-%d", i/mediaLeaseRegistryMaxPerToken)
		if err := registry.Register(MediaLease{GatewayTokenHash: owner, PlaySessionID: PlaySessionID(fmt.Sprintf("p-%d", i))}); err != nil {
			t.Fatalf("fill global capacity at %d: %v", i, err)
		}
	}
	if err := registry.Register(MediaLease{GatewayTokenHash: "extra", PlaySessionID: "overflow"}); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("global overflow = %v, want ErrStoreUnavailable", err)
	}
}

func TestMediaLeaseAtomicPairRollback(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	registry := NewMediaLeaseRegistry(func() time.Time { return now })
	if err := registry.Register(MediaLease{GatewayTokenHash: "first", LiveStreamID: "occupied"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(MediaLease{GatewayTokenHash: "second", PlaySessionID: "new", LiveStreamID: "occupied"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ownership conflict = %v, want ErrNotFound", err)
	}
	if _, err := registry.Validate("second", "new", "", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial play lease survived conflict: %v", err)
	}

	registry = NewMediaLeaseRegistry(func() time.Time { return now })
	for i := 0; i < mediaLeaseRegistryMaxPerToken-1; i++ {
		if err := registry.Register(MediaLease{GatewayTokenHash: "full", PlaySessionID: PlaySessionID(fmt.Sprintf("existing-%d", i))}); err != nil {
			t.Fatalf("fill pair capacity at %d: %v", i, err)
		}
	}
	if err := registry.Register(MediaLease{GatewayTokenHash: "full", PlaySessionID: "pair-play", LiveStreamID: "pair-live"}); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("pair capacity error = %v, want ErrStoreUnavailable", err)
	}
	if _, err := registry.Validate("full", "pair-play", "", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial play lease survived capacity failure: %v", err)
	}
	if _, err := registry.Validate("full", "", "pair-live", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial live lease survived capacity failure: %v", err)
	}
}

func TestMediaLeaseRemoveSessionAndSweep(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	registry := NewMediaLeaseRegistry(func() time.Time { return now })
	if err := registry.Register(MediaLease{GatewayTokenHash: "remove", PlaySessionID: "p", LiveStreamID: "l"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(MediaLease{GatewayTokenHash: "keep", PlaySessionID: "k"}); err != nil {
		t.Fatal(err)
	}
	registry.RemoveSession("remove")
	if _, err := registry.Validate("remove", "p", "", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed play lease: %v", err)
	}
	if _, err := registry.Validate("remove", "", "l", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed live lease: %v", err)
	}
	if _, err := registry.Validate("keep", "k", "", now); err != nil {
		t.Fatalf("unrelated lease removed: %v", err)
	}
	if got := registry.Sweep(now.Add(mediaLeaseTTL)); got != 1 {
		t.Fatalf("sweep removed %d leases, want 1", got)
	}
}

func TestMediaLeaseConcurrentCapacityAndOwnership(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	registry := NewMediaLeaseRegistry(func() time.Time { return now })
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 256; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := registry.Register(MediaLease{GatewayTokenHash: "owner", PlaySessionID: PlaySessionID(fmt.Sprintf("id-%d", i))})
			if err == nil {
				admitted.Add(1)
				return
			}
			if !errors.Is(err, ErrStoreUnavailable) {
				t.Errorf("concurrent capacity error = %v", err)
			}
		}(i)
	}
	wg.Wait()
	if got := admitted.Load(); got != mediaLeaseRegistryMaxPerToken {
		t.Fatalf("admitted %d leases, want %d", got, mediaLeaseRegistryMaxPerToken)
	}

	registry = NewMediaLeaseRegistry(func() time.Time { return now })
	var winners atomic.Int64
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := registry.Register(MediaLease{GatewayTokenHash: fmt.Sprintf("owner-%d", i), LiveStreamID: "shared"})
			if err == nil {
				winners.Add(1)
				return
			}
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("concurrent ownership error = %v", err)
			}
		}(i)
	}
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("ownership winners = %d, want 1", got)
	}
}

func TestMediaLeaseAtomicReleaseRestoresCapacity(t *testing.T) {
	registry := NewMediaLeaseRegistry(nil)
	for i := 0; i < mediaLeaseRegistryMaxPerToken; i++ {
		if err := registry.Register(MediaLease{GatewayTokenHash: "owner", PlaySessionID: PlaySessionID(fmt.Sprintf("id-%d", i))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := registry.Release("owner", []PlaySessionID{"id-0", "id-1"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterAll("owner", []PlaySessionID{"replacement-play"}, []LiveStreamID{"replacement-live"}); err != nil {
		t.Fatalf("released capacity was not reusable: %v", err)
	}
	if err := registry.ValidateAll("owner", []PlaySessionID{"replacement-play"}, []LiveStreamID{"replacement-live"}, time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestMediaLeaseReleaseIsAtomicForForeignOrMissingIdentifiers(t *testing.T) {
	registry := NewMediaLeaseRegistry(nil)
	if err := registry.RegisterAll("owner", []PlaySessionID{"play-a", "play-b"}, []LiveStreamID{"live-a"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(MediaLease{GatewayTokenHash: "foreign", LiveStreamID: "foreign-live"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Release("owner", []PlaySessionID{"play-a"}, []LiveStreamID{"foreign-live"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("release err=%v", err)
	}
	if err := registry.ValidateAll("owner", []PlaySessionID{"play-a", "play-b"}, []LiveStreamID{"live-a"}, time.Time{}); err != nil {
		t.Fatalf("failed release partially removed identifiers: %v", err)
	}
}
