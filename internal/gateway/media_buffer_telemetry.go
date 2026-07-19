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
