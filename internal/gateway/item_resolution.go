package gateway

import "time"

type resolutionOutcome int

const (
	resolutionMissing resolutionOutcome = iota
	resolutionFingerprintMismatch
	resolutionKeep
)

// reconcileResolvedItem mutates state for resolution side effects and returns outcome.
// - missing item: set OrphanedAt=now
// - fingerprint incompatible: set OrphanedAt=now
// - keep: clear OrphanedAt, set LastSeenAt=now, mergeItemMetadata(state, item)
func reconcileResolvedItem(state *PlaybackState, item map[string]any, present bool, now time.Time) resolutionOutcome {
	if state == nil {
		return resolutionMissing
	}
	if !present {
		state.OrphanedAt = &now
		return resolutionMissing
	}
	fingerprint := itemFingerprint(item)
	if state.Fingerprint != "" && fingerprint != "" && !fingerprintsCompatible(state.Fingerprint, fingerprint) {
		state.OrphanedAt = &now
		return resolutionFingerprintMismatch
	}
	state.OrphanedAt = nil
	state.LastSeenAt = &now
	mergeItemMetadata(state, item)
	return resolutionKeep
}
