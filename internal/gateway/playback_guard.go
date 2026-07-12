package gateway

import (
	"sync"
	"time"
)

const (
	playbackGuardTTL      = 2 * time.Minute
	playbackAuditCooldown = 2 * time.Minute
)

type playbackGuardKey struct {
	GatewayTokenHash string
	ItemID           string
}

type playbackGuard struct {
	generation uint64
	expiresAt  time.Time
}

type playbackAuditKey struct {
	guard playbackGuardKey
	event string
}

// playbackGuardTracker is process-local correlation state for denied playback attempts.
type playbackGuardTracker struct {
	mu         sync.Mutex
	guards     map[playbackGuardKey]playbackGuard
	cooldowns  map[playbackAuditKey]time.Time
	generation uint64
	now        func() time.Time
}

func newPlaybackGuardTracker() *playbackGuardTracker {
	return &playbackGuardTracker{
		guards:    map[playbackGuardKey]playbackGuard{},
		cooldowns: map[playbackAuditKey]time.Time{},
		now:       time.Now,
	}
}

func (t *playbackGuardTracker) snapshot(key playbackGuardKey) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.sweep(now)
	return t.guards[key].generation
}

func (t *playbackGuardTracker) deny(key playbackGuardKey) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.sweep(now)
	t.generation++
	t.guards[key] = playbackGuard{generation: t.generation, expiresAt: now.Add(playbackGuardTTL)}
	return t.auditEligible(key, "playback_concurrency_denied", now)
}

func (t *playbackGuardTracker) clearIfGeneration(key playbackGuardKey, generation uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.sweep(now)
	if generation == 0 {
		return
	}
	if guard, ok := t.guards[key]; ok && guard.generation == generation {
		delete(t.guards, key)
	}
}

func (t *playbackGuardTracker) suppress(key playbackGuardKey) (active, auditEligible bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.sweep(now)
	if _, active = t.guards[key]; !active {
		return false, false
	}
	return true, t.auditEligible(key, "playback_report_suppressed", now)
}

func (t *playbackGuardTracker) auditEligible(key playbackGuardKey, event string, now time.Time) bool {
	auditKey := playbackAuditKey{guard: key, event: event}
	if until, ok := t.cooldowns[auditKey]; ok && now.Before(until) {
		return false
	}
	t.cooldowns[auditKey] = now.Add(playbackAuditCooldown)
	return true
}

func (t *playbackGuardTracker) sweep(now time.Time) {
	for key, guard := range t.guards {
		if !now.Before(guard.expiresAt) {
			delete(t.guards, key)
		}
	}
	for key, until := range t.cooldowns {
		if !now.Before(until) {
			delete(t.cooldowns, key)
		}
	}
}
