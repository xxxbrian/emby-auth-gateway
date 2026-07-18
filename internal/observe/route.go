package observe

import "strings"

// ClassifyRoute maps an HTTP method and Emby-relative path to a low-cardinality
// route class suitable for series labels. Raw paths must never be used as labels.
func ClassifyRoute(method, relPath string) string {
	_ = method
	path := relPath
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return RouteOther
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	lower := strings.ToLower(path)

	switch {
	case strings.Contains(lower, "authenticatebyname"),
		strings.Contains(lower, "authenticate"),
		strings.HasSuffix(lower, "/sessions/logout"),
		lower == "/sessions/logout",
		strings.Contains(lower, "/connect/"):
		return RouteAuth
	case strings.Contains(lower, "playbackinfo"),
		strings.Contains(lower, "/playingitems"),
		strings.Contains(lower, "playbackstarted"),
		strings.Contains(lower, "playbackstopped"),
		strings.Contains(lower, "playbackprogress"),
		strings.Contains(lower, "/sessions/playing"):
		return RoutePlayback
	case strings.Contains(lower, "/videos/"),
		strings.Contains(lower, "/audio/"),
		strings.Contains(lower, "/livestreams"),
		strings.HasSuffix(lower, "/download"),
		strings.Contains(lower, "/hls/"),
		strings.Contains(lower, "/stream"),
		strings.HasSuffix(lower, ".m3u8"),
		strings.HasSuffix(lower, ".ts"):
		return RouteMedia
	case strings.Contains(lower, "userdata"),
		strings.Contains(lower, "favoriteitems"),
		strings.Contains(lower, "playeditems"),
		strings.Contains(lower, "hidefromresume"):
		return RouteUserdata
	case strings.Contains(lower, "/items"),
		strings.Contains(lower, "/users/"),
		strings.Contains(lower, "/genres"),
		strings.Contains(lower, "/studios"),
		strings.Contains(lower, "/persons"),
		strings.Contains(lower, "/shows"),
		strings.Contains(lower, "/movies"),
		strings.Contains(lower, "/artists"),
		strings.Contains(lower, "/system/info"),
		strings.Contains(lower, "/displaypreferences"):
		return RouteMetadata
	default:
		return RouteOther
	}
}

// StatusClassOf maps an HTTP status code to a low-cardinality class.
func StatusClassOf(status int) string {
	switch {
	case status <= 0:
		return Status0
	case status < 300:
		return Status2xx
	case status < 400:
		return Status3xx
	case status < 500:
		return Status4xx
	default:
		return Status5xx
	}
}
