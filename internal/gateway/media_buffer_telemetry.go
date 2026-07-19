package gateway

import "github.com/xxxbrian/emby-auth-gateway/internal/telemetry"

// MediaBufferSnapshot returns bounded aggregate media-buffer state.
func (s *Server) MediaBufferSnapshot() telemetry.MediaBufferStatus {
	if s == nil || s.mediaBuffer == nil {
		return telemetry.MediaBufferStatus{}
	}
	snapshot := s.mediaBuffer.Snapshot()
	return telemetry.MediaBufferStatus{
		Enabled:          snapshot.Enabled,
		HardBudgetBytes:  snapshot.HardBudget,
		AllocatedBytes:   snapshot.Allocated,
		OwnedBytes:       snapshot.Owned,
		FreeBytes:        snapshot.Free,
		ActiveRequests:   snapshot.ActiveRequests,
		BaseOnlyRequests: snapshot.BaseOnlyRequests,
		IndebtedRequests: snapshot.IndebtedRequests,
		RequestDebtBytes: snapshot.RequestDebtBytes,
	}
}

// MediaBufferControllerSnapshot adapts the controller's locked O(1) snapshot
// to the narrow telemetry provider contract.
func (s *Server) MediaBufferControllerSnapshot() telemetry.MediaBufferControllerSnapshot {
	if s == nil || s.mediaBuffer == nil {
		return telemetry.MediaBufferControllerSnapshot{}
	}
	snapshot := s.mediaBuffer.Snapshot()
	return telemetry.MediaBufferControllerSnapshot{
		Enabled:                  snapshot.Enabled,
		Available:                true,
		HardBudgetBytes:          snapshot.HardBudget,
		AllocatedBytes:           snapshot.Allocated,
		OwnedBytes:               snapshot.Owned,
		FreeBytes:                snapshot.Free,
		UnallocatedOptionalBytes: snapshot.HardBudget - snapshot.Allocated,
		PrivateBaseBytes:         int64(snapshot.ActiveRequests) * mediaBufferChunkSize,
		ActiveRequests:           snapshot.ActiveRequests,
		BaseOnlyRequests:         snapshot.BaseOnlyRequests,
		IndebtedRequests:         snapshot.IndebtedRequests,
		RequestDebtBytes:         snapshot.RequestDebtBytes,
	}
}
