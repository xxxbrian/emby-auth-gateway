package telemetry

import "github.com/xxxbrian/emby-auth-gateway/internal/subtitles"

// SetSubtitlesProvider is composed at startup. The provider returns a read-only
// in-memory snapshot; observing subtitles must never request or prepare media.
func (r *Registry) SetSubtitlesProvider(provider func() subtitles.Snapshot) {
	if r == nil {
		return
	}
	r.subtitleMu.Lock()
	r.subtitleProvider = provider
	r.subtitleMu.Unlock()
}

func (r *Registry) SubtitleSnapshot() subtitles.Snapshot {
	if r == nil {
		return subtitles.Snapshot{Jobs: []subtitles.JobView{}}
	}
	r.subtitleMu.RLock()
	provider := r.subtitleProvider
	r.subtitleMu.RUnlock()
	if provider == nil {
		return subtitles.Snapshot{BootID: r.bootID, Jobs: []subtitles.JobView{}}
	}
	snapshot := provider()
	if snapshot.BootID == "" {
		snapshot.BootID = r.bootID
	}
	if snapshot.Jobs == nil {
		snapshot.Jobs = []subtitles.JobView{}
	}
	return snapshot
}
