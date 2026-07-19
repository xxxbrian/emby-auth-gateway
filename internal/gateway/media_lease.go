package gateway

import (
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	mediaLeaseIdentifierMaxBytes = 512
	mediaLeaseOwnerMaxBytes      = 128
)

type mediaLeaseKind uint8

const (
	mediaLeaseKindPlaySession mediaLeaseKind = iota + 1
	mediaLeaseKindLiveStream
)

type mediaLeaseKey struct {
	kind mediaLeaseKind
	id   string
}

type mediaLeaseEntry struct {
	owner     string
	expiresAt time.Time
}

type mediaLeaseRegistry struct {
	mu       sync.Mutex
	clock    func() time.Time
	leases   map[mediaLeaseKey]mediaLeaseEntry
	perToken map[string]int
}

// NewMediaLeaseRegistry creates bounded process-local lease state. A nil clock
// uses UTC wall time; callers may inject a clock for deterministic operation.
func NewMediaLeaseRegistry(clock func() time.Time) MediaLeaseRegistry {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &mediaLeaseRegistry{
		clock:    clock,
		leases:   make(map[mediaLeaseKey]mediaLeaseEntry),
		perToken: make(map[string]int),
	}
}

func (r *mediaLeaseRegistry) Register(lease MediaLease) error {
	return r.RegisterAll(lease.GatewayTokenHash, leaseIDs(lease.PlaySessionID), liveLeaseIDs(lease.LiveStreamID))
}

func (r *mediaLeaseRegistry) RegisterAll(gatewayTokenHash string, playSessionIDs []PlaySessionID, liveStreamIDs []LiveStreamID) error {
	owner := strings.TrimSpace(gatewayTokenHash)
	keys, ok := mediaLeaseKeySet(playSessionIDs, liveStreamIDs)
	if !validMediaLeaseOwner(owner, gatewayTokenHash) || !ok {
		return ErrBadRequest
	}

	now := r.clock().UTC()
	expiresAt := now.Add(mediaLeaseTTL)

	r.mu.Lock()
	defer r.mu.Unlock()

	r.sweepLocked(now)
	newCount := 0
	for _, key := range keys {
		entry, exists := r.leases[key]
		if exists && entry.owner != owner {
			return ErrNotFound
		}
		if !exists {
			newCount++
		}
	}
	if len(r.leases)+newCount > mediaLeaseRegistryMaxGlobal || r.perToken[owner]+newCount > mediaLeaseRegistryMaxPerToken {
		return ErrStoreUnavailable
	}

	for _, key := range keys {
		if _, exists := r.leases[key]; !exists {
			r.perToken[owner]++
		}
		r.leases[key] = mediaLeaseEntry{owner: owner, expiresAt: expiresAt}
	}

	return nil
}

func (r *mediaLeaseRegistry) Validate(gatewayTokenHash string, playSessionID PlaySessionID, liveStreamID LiveStreamID, now time.Time) (MediaLease, error) {
	owner := strings.TrimSpace(gatewayTokenHash)
	keys, playSessionID, liveStreamID, ok := mediaLeaseKeys(playSessionID, liveStreamID)
	if !validMediaLeaseOwner(owner, gatewayTokenHash) || !ok {
		return MediaLease{}, ErrNotFound
	}
	if now.IsZero() {
		now = r.clock()
	}
	now = now.UTC()

	r.mu.Lock()
	defer r.mu.Unlock()

	var expiresAt time.Time
	for _, key := range keys {
		entry, exists := r.leases[key]
		if exists && !entry.expiresAt.After(now) {
			r.removeLocked(key, entry)
			exists = false
		}
		if !exists || entry.owner != owner {
			return MediaLease{}, ErrNotFound
		}
		if expiresAt.IsZero() || entry.expiresAt.Before(expiresAt) {
			expiresAt = entry.expiresAt
		}
	}

	return MediaLease{
		GatewayTokenHash: owner,
		PlaySessionID:    playSessionID,
		LiveStreamID:     liveStreamID,
		ExpiresAt:        expiresAt,
	}, nil
}

func (r *mediaLeaseRegistry) ValidateAll(gatewayTokenHash string, playSessionIDs []PlaySessionID, liveStreamIDs []LiveStreamID, now time.Time) error {
	owner := strings.TrimSpace(gatewayTokenHash)
	keys, ok := mediaLeaseKeySet(playSessionIDs, liveStreamIDs)
	if !validMediaLeaseOwner(owner, gatewayTokenHash) || !ok {
		return ErrNotFound
	}
	if now.IsZero() {
		now = r.clock()
	}
	now = now.UTC()

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range keys {
		entry, exists := r.leases[key]
		if exists && !entry.expiresAt.After(now) {
			r.removeLocked(key, entry)
			exists = false
		}
		if !exists || entry.owner != owner {
			return ErrNotFound
		}
	}
	return nil
}

func (r *mediaLeaseRegistry) Release(gatewayTokenHash string, playSessionIDs []PlaySessionID, liveStreamIDs []LiveStreamID) error {
	owner := strings.TrimSpace(gatewayTokenHash)
	keys, ok := mediaLeaseKeySet(playSessionIDs, liveStreamIDs)
	if !validMediaLeaseOwner(owner, gatewayTokenHash) || !ok {
		return ErrNotFound
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock().UTC()
	for _, key := range keys {
		entry, exists := r.leases[key]
		if exists && !entry.expiresAt.After(now) {
			r.removeLocked(key, entry)
			exists = false
		}
		if !exists || entry.owner != owner {
			return ErrNotFound
		}
	}
	for _, key := range keys {
		r.removeLocked(key, r.leases[key])
	}
	return nil
}

func (r *mediaLeaseRegistry) RemoveSession(gatewayTokenHash string) {
	owner := strings.TrimSpace(gatewayTokenHash)
	if !validMediaLeaseOwner(owner, gatewayTokenHash) {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for key, entry := range r.leases {
		if entry.owner == owner {
			r.removeLocked(key, entry)
		}
	}
}

func (r *mediaLeaseRegistry) Owners() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	owners := make([]string, 0, len(r.perToken))
	for owner := range r.perToken {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	return owners
}

func (r *mediaLeaseRegistry) Sweep(now time.Time) int {
	if now.IsZero() {
		now = r.clock()
	}
	now = now.UTC()

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sweepLocked(now)
}

func (r *mediaLeaseRegistry) sweepLocked(now time.Time) int {
	removed := 0
	for key, entry := range r.leases {
		if !entry.expiresAt.After(now) {
			r.removeLocked(key, entry)
			removed++
		}
	}
	return removed
}

func (r *mediaLeaseRegistry) removeLocked(key mediaLeaseKey, entry mediaLeaseEntry) {
	delete(r.leases, key)
	remaining := r.perToken[entry.owner] - 1
	if remaining <= 0 {
		delete(r.perToken, entry.owner)
		return
	}
	r.perToken[entry.owner] = remaining
}

func mediaLeaseKeys(playSessionID PlaySessionID, liveStreamID LiveStreamID) ([]mediaLeaseKey, PlaySessionID, LiveStreamID, bool) {
	play := strings.TrimSpace(string(playSessionID))
	live := strings.TrimSpace(string(liveStreamID))
	if (play == "" && live == "") || len(play) > mediaLeaseIdentifierMaxBytes || len(live) > mediaLeaseIdentifierMaxBytes {
		return nil, "", "", false
	}

	keys := make([]mediaLeaseKey, 0, 2)
	if play != "" {
		keys = append(keys, mediaLeaseKey{kind: mediaLeaseKindPlaySession, id: play})
	}
	if live != "" {
		keys = append(keys, mediaLeaseKey{kind: mediaLeaseKindLiveStream, id: live})
	}
	return keys, PlaySessionID(play), LiveStreamID(live), true
}

func mediaLeaseKeySet(playSessionIDs []PlaySessionID, liveStreamIDs []LiveStreamID) ([]mediaLeaseKey, bool) {
	keys := make([]mediaLeaseKey, 0, len(playSessionIDs)+len(liveStreamIDs))
	seen := make(map[mediaLeaseKey]struct{}, cap(keys))
	for _, id := range playSessionIDs {
		value := strings.TrimSpace(string(id))
		if value == "" || len(value) > mediaLeaseIdentifierMaxBytes {
			return nil, false
		}
		key := mediaLeaseKey{kind: mediaLeaseKindPlaySession, id: value}
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	for _, id := range liveStreamIDs {
		value := strings.TrimSpace(string(id))
		if value == "" || len(value) > mediaLeaseIdentifierMaxBytes {
			return nil, false
		}
		key := mediaLeaseKey{kind: mediaLeaseKindLiveStream, id: value}
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	return keys, len(keys) != 0
}

func leaseIDs(id PlaySessionID) []PlaySessionID {
	if id == "" {
		return nil
	}
	return []PlaySessionID{id}
}

func liveLeaseIDs(id LiveStreamID) []LiveStreamID {
	if id == "" {
		return nil
	}
	return []LiveStreamID{id}
}

func validMediaLeaseOwner(trimmed, original string) bool {
	return trimmed != "" && trimmed == original && len(trimmed) <= mediaLeaseOwnerMaxBytes
}

var _ MediaLeaseRegistry = (*mediaLeaseRegistry)(nil)
