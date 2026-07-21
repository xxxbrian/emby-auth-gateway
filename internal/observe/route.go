package observe

import (
	"strings"

	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
)

// RouteClassOf maps a routeclass decision to a low-cardinality telemetry label.
// Raw paths must never be used as labels.
func RouteClassOf(decision routeclass.Decision) string {
	switch decision.Ownership {
	case routeclass.LocalPublic:
		switch decision.Operation {
		case routeclass.OperationAuthenticate:
			return RouteAuth
		default:
			return RouteMetadata
		}
	case routeclass.LocalSession:
		switch decision.Operation {
		case routeclass.OperationLogout:
			return RouteAuth
		default:
			return RoutePlayback
		}
	case routeclass.LocalPersonal:
		switch decision.Operation {
		case routeclass.OperationCurrentUser:
			return RouteMetadata
		default:
			return RouteUserdata
		}
	case routeclass.MetadataProxy:
		return RouteMetadata
	case routeclass.MediaProxy:
		return RouteMedia
	case routeclass.DeniedSession:
		return RoutePlayback
	case routeclass.Unclassified:
		// Default-deny classifier outcome.
		return RouteOther
	default:
		return RouteOther
	}
}

// ClassifyRoute maps an HTTP method and path-like input to a low-cardinality
// route class suitable for series labels.
//
// Compatibility wrapper: arbitrary inputs may include query/fragment or
// surrounding whitespace. Those are sanitized here before routeclass.Classify,
// which accepts path-only values and must not perform that sanitization itself.
func ClassifyRoute(method, relPath string) string {
	return RouteClassOf(routeclass.Classify(method, sanitizeCompatPath(relPath)))
}

// sanitizeCompatPath strips query/fragment and trims whitespace for older
// telemetry callers that may pass non-path URI fragments. Live gateway dispatch
// must pass path-only values directly to routeclass.Classify.
func sanitizeCompatPath(relPath string) string {
	path := relPath
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	return strings.TrimSpace(path)
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
