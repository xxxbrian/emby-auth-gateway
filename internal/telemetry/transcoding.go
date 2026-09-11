package telemetry

import "github.com/xxxbrian/emby-auth-gateway/internal/transcode"

// SetTranscodingProvider is composed at startup. The provider reads an immutable
// projection; Registry never uses it to control conversion or fetch media.
func (r *Registry) SetTranscodingProvider(provider func() transcode.Observation) {
	if r == nil {
		return
	}
	r.transcodeMu.Lock()
	r.transcodeProvider = provider
	r.transcodeMu.Unlock()
}
func (r *Registry) TranscodingSnapshot() transcode.Observation {
	if r == nil {
		return transcode.Observation{Jobs: []transcode.JobView{}, Recent: []transcode.JobView{}}
	}
	r.transcodeMu.RLock()
	provider := r.transcodeProvider
	r.transcodeMu.RUnlock()
	if provider == nil {
		return transcode.Observation{BootID: r.bootID, At: r.now(), Jobs: []transcode.JobView{}, Recent: []transcode.JobView{}}
	}
	return provider()
}
