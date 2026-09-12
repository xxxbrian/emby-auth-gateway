package gateway

import (
	"context"
	"net/http"
)

type observedMediaSourceKey struct{}

func withObservedMediaSource(r *http.Request, source upstreamRequestSnapshot) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), observedMediaSourceKey{}, MediaSourceRef(source.serverID, source.userID)))
}

func observedMediaSource(r *http.Request) string {
	if r == nil {
		return ""
	}
	ref, _ := r.Context().Value(observedMediaSourceKey{}).(string)
	return ref
}

// localMediaSource reads the pinned source without performing authentication or
// network requests. It is only used at local playback-report capture, never in
// media copy operations or under a controller/queue lock.
func (s *Server) localMediaSource(ctx context.Context) string {
	runtime, err := s.store.LoadDefaultUpstreamRuntime(ctx)
	if err != nil || runtime == nil {
		return ""
	}
	return MediaSourceRef(runtime.Source.ServerID, runtime.Source.BackendUserID)
}
