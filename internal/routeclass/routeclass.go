// Package routeclass classifies Emby-relative HTTP routes by architectural ownership.
//
// It is a stdlib-only leaf package shared by gateway dispatch and observe telemetry.
// Path matching is case-insensitive and never embeds raw user IDs in outputs.
//
// Callers must pass a path-only value (for example net/http Request.URL.Path or the
// gateway Emby-relative path). Never pass RawQuery or a full request URI; query and
// fragment characters that appear in the path string are treated as path data.
package routeclass

import (
	"strings"
)

// Ownership is the architectural owner of a classified route.
type Ownership uint8

const (
	LocalPublic Ownership = iota + 1
	LocalPersonal
	LocalSession
	MetadataProxy
	MediaProxy
	DeniedSession
	LegacyProxy
)

// Operation identifies the recognized handler family for a decision.
type Operation uint8

const (
	OperationLegacyProxy Operation = iota
	OperationAuthenticate
	OperationPublicSystemInfo
	OperationPing
	OperationLogout
	OperationPublicUsers
	OperationCurrentUser
	OperationBrandingConfiguration
	OperationBrandingCSS
	OperationPersonal
	OperationSessionList
	OperationPlaybackReport
	OperationPlaybackPing
	OperationCapabilities
	OperationDeniedSession
	OperationMetadataProxy
	OperationMediaProxy
	OperationPlaybackInfo
	OperationLiveStreamOpen
	OperationLiveStreamMediaInfo
	OperationLiveStreamClose
	OperationActiveEncodingsDelete
	OperationActiveEncodingsDeleteCompat
	OperationWebSocket
	OperationSessionGeneralCommand
	OperationSessionPlay
	OperationSessionPlaystate
)

// Decision is the pure classification result for one method+path pair.
type Decision struct {
	Ownership     Ownership
	Operation     Operation
	MethodAllowed bool
	Allow         string
}

// Classify returns the ownership decision for an Emby-relative path-only value.
//
// relPath must already be a path (URL.Path / gateway relative path), not a URI with
// RawQuery. Normalization is limited to matching gateway handler path equality:
// ensure a leading slash, trim trailing slashes except root, and match segments
// case-insensitively. Characters such as '?', '#', and surrounding whitespace are
// path data and are preserved; they prevent exact local Session matches the same
// way they would for equalPath-style handlers. Outputs never include raw path
// identifiers.
func Classify(method, relPath string) Decision {
	method = strings.ToUpper(strings.TrimSpace(method))
	parts := pathParts(normalizePath(relPath))

	if d, ok := classifyExact(method, parts); ok {
		return d
	}
	if d, ok := classifyNegotiation(method, parts); ok {
		return d
	}
	if d, ok := classifySessions(method, parts); ok {
		return d
	}
	if d, ok := classifyPersonal(parts); ok {
		return d
	}
	if d, ok := classifyMedia(method, parts); ok {
		return d
	}
	if d, ok := classifyMetadata(method, parts); ok {
		return d
	}
	return Decision{
		Ownership:     LegacyProxy,
		Operation:     OperationLegacyProxy,
		MethodAllowed: true,
	}
}

func classifyExact(method string, parts []string) (Decision, bool) {
	switch {
	case len(parts) == 2 && parts[0] == "users" && parts[1] == "authenticatebyname":
		return methodDecision(method, LocalPublic, OperationAuthenticate, "POST"), true
	case len(parts) == 3 && parts[0] == "system" && parts[1] == "info" && parts[2] == "public":
		return methodDecision(method, LocalPublic, OperationPublicSystemInfo, "GET"), true
	case len(parts) == 2 && parts[0] == "system" && parts[1] == "ping":
		return methodDecision(method, LocalPublic, OperationPing, "GET", "POST"), true
	case len(parts) == 2 && parts[0] == "users" && parts[1] == "public":
		return methodDecision(method, LocalPublic, OperationPublicUsers, "GET"), true
	case len(parts) == 2 && parts[0] == "branding" && parts[1] == "configuration":
		return methodDecision(method, LocalPublic, OperationBrandingConfiguration, "GET"), true
	case len(parts) == 2 && parts[0] == "branding" && parts[1] == "css.css":
		return methodDecision(method, LocalPublic, OperationBrandingCSS, "GET"), true
	case len(parts) == 1 && parts[0] == "embywebsocket":
		return methodDecision(method, LocalSession, OperationWebSocket, "GET"), true
	case len(parts) == 2 && parts[0] == "users" && parts[1] != "":
		// Single-user shape: /Users/{id} or /Users/Me. Deeper user paths are not current-user.
		return methodDecision(method, LocalPersonal, OperationCurrentUser, "GET"), true
	default:
		return Decision{}, false
	}
}

func classifyNegotiation(method string, parts []string) (Decision, bool) {
	switch {
	case len(parts) == 3 && parts[0] == "items" && parts[1] != "" && parts[2] == "playbackinfo":
		return methodDecision(method, MediaProxy, OperationPlaybackInfo, "GET", "POST"), true
	case len(parts) == 2 && parts[0] == "livestreams" && parts[1] == "open":
		return methodDecision(method, MediaProxy, OperationLiveStreamOpen, "POST"), true
	case len(parts) == 2 && parts[0] == "livestreams" && parts[1] == "mediainfo":
		return methodDecision(method, MediaProxy, OperationLiveStreamMediaInfo, "POST"), true
	case len(parts) == 2 && parts[0] == "livestreams" && parts[1] == "close":
		return methodDecision(method, MediaProxy, OperationLiveStreamClose, "POST"), true
	case len(parts) == 2 && parts[0] == "videos" && parts[1] == "activeencodings":
		return methodDecision(method, MediaProxy, OperationActiveEncodingsDelete, "DELETE"), true
	case len(parts) == 3 && parts[0] == "videos" && parts[1] == "activeencodings" && parts[2] == "delete":
		return methodDecision(method, MediaProxy, OperationActiveEncodingsDeleteCompat, "POST"), true
	default:
		return Decision{}, false
	}
}

func classifySessions(method string, parts []string) (Decision, bool) {
	if len(parts) == 0 || parts[0] != "sessions" {
		return Decision{}, false
	}

	// Exact local Session routes first.
	switch {
	case len(parts) == 1:
		return methodDecision(method, LocalSession, OperationSessionList, "GET"), true
	case len(parts) == 2 && parts[1] == "logout":
		return methodDecision(method, LocalSession, OperationLogout, "POST"), true
	case len(parts) == 2 && parts[1] == "playing":
		return methodDecision(method, LocalSession, OperationPlaybackReport, "POST"), true
	case len(parts) == 3 && parts[1] == "playing" && (parts[2] == "progress" || parts[2] == "stopped"):
		return methodDecision(method, LocalSession, OperationPlaybackReport, "POST"), true
	case len(parts) == 3 && parts[1] == "playing" && parts[2] == "ping":
		return methodDecision(method, LocalSession, OperationPlaybackPing, "POST"), true
	case len(parts) == 2 && parts[1] == "capabilities":
		return methodDecision(method, LocalSession, OperationCapabilities, "POST"), true
	case len(parts) == 3 && parts[1] == "capabilities" && parts[2] == "full":
		return methodDecision(method, LocalSession, OperationCapabilities, "POST"), true
	case len(parts) == 2 && parts[1] == "playqueue":
		return methodDecision(method, DeniedSession, OperationDeniedSession, "GET"), true
	}

	// Official targeted Session control families (still denied; wrong methods -> 405).
	if d, ok := classifyTargetedSession(method, parts); ok {
		return d, true
	}

	// Any remaining /Sessions/{...} path is denied without method restriction (403).
	if len(parts) >= 2 {
		return Decision{
			Ownership:     DeniedSession,
			Operation:     OperationDeniedSession,
			MethodAllowed: true,
		}, true
	}
	return Decision{}, false
}

func classifyTargetedSession(method string, parts []string) (Decision, bool) {
	// /Sessions/{id}/...
	if len(parts) < 3 {
		return Decision{}, false
	}
	// parts[1] is the session id segment; do not retain it.
	rest := parts[2:]

	switch {
	// /Sessions/{id}/Playing and /Sessions/{id}/Playing/{command}
	case rest[0] == "playing" && len(rest) == 1:
		return methodDecision(method, LocalSession, OperationSessionPlay, "POST"), true
	case rest[0] == "playing" && len(rest) == 2:
		return methodDecision(method, LocalSession, OperationSessionPlaystate, "POST"), true
	// /Sessions/{id}/Command and /Sessions/{id}/Command/{command}
	case rest[0] == "command" && (len(rest) == 1 || len(rest) == 2):
		return methodDecision(method, LocalSession, OperationSessionGeneralCommand, "POST"), true
	// /Sessions/{id}/System/{command}
	case rest[0] == "system" && len(rest) == 2:
		return methodDecision(method, DeniedSession, OperationDeniedSession, "POST"), true
	// /Sessions/{id}/Message
	case rest[0] == "message" && len(rest) == 1:
		return methodDecision(method, DeniedSession, OperationDeniedSession, "POST"), true
	// /Sessions/{id}/Viewing
	case rest[0] == "viewing" && len(rest) == 1:
		return methodDecision(method, DeniedSession, OperationDeniedSession, "POST"), true
	// /Sessions/{id}/Users/{userId}
	case rest[0] == "users" && len(rest) == 2:
		return methodDecision(method, DeniedSession, OperationDeniedSession, "POST", "DELETE"), true
	// /Sessions/{id}/Users/{userId}/Delete
	case rest[0] == "users" && len(rest) == 3 && rest[2] == "delete":
		return methodDecision(method, DeniedSession, OperationDeniedSession, "POST"), true
	default:
		return Decision{}, false
	}
}

func classifyPersonal(parts []string) (Decision, bool) {
	switch {
	// Display preferences: /DisplayPreferences/{id}
	case len(parts) >= 1 && parts[0] == "displaypreferences":
		return personal(), true
	// NextUp: /Shows/NextUp
	case len(parts) == 2 && parts[0] == "shows" && parts[1] == "nextup":
		return personal(), true
	// User-scoped personal paths under /Users/{id}/...
	case len(parts) >= 3 && parts[0] == "users":
		tail := parts[2:]
		switch {
		case len(tail) >= 1 && (tail[0] == "playeditems" || tail[0] == "favoriteitems" || tail[0] == "hidefromresume"):
			return personal(), true
		case len(tail) >= 2 && tail[0] == "items" && (tail[1] == "resume" || tail[1] == "latest"):
			return personal(), true
		case len(tail) == 3 && tail[0] == "items" && (tail[2] == "rating" || tail[2] == "userdata"):
			return personal(), true
		case len(tail) >= 1 && tail[0] == "items" && hasPersonalListShape(tail):
			// Keep plain /Users/{id}/Items as metadata; only treat clearly local personal list shapes here.
			return personal(), true
		default:
			return Decision{}, false
		}
	default:
		return Decision{}, false
	}
}

func hasPersonalListShape(tail []string) bool {
	// /Users/{id}/Items/Resume|Latest already handled. Additional personal-only list suffixes.
	if len(tail) < 2 {
		return false
	}
	switch tail[1] {
	case "resume", "latest":
		return true
	default:
		return false
	}
}

func classifyMedia(method string, parts []string) (Decision, bool) {
	if len(parts) == 0 {
		return Decision{}, false
	}
	joined := strings.Join(parts, "/")
	switch {
	case len(parts) >= 3 && parts[0] == "items" && parts[2] == "images":
		return media(method), true
	case len(parts) >= 3 && parts[0] == "users" && parts[2] == "images":
		return media(method), true
	case parts[0] == "videos", parts[0] == "audio", parts[0] == "livestreams", parts[0] == "hls":
		return media(method), true
	case strings.HasSuffix(joined, "/download"):
		return media(method), true
	case strings.Contains(joined, "/stream"):
		return media(method), true
	case strings.HasSuffix(joined, ".m3u8"), strings.HasSuffix(joined, ".ts"):
		return media(method), true
	default:
		return Decision{}, false
	}
}

func classifyMetadata(method string, parts []string) (Decision, bool) {
	if len(parts) == 0 {
		return Decision{}, false
	}
	switch parts[0] {
	case "items", "genres", "studios", "persons", "shows", "movies", "artists", "trailers", "live", "channels", "scheduledtasks":
		return metadata(method), true
	case "users":
		// Remaining user-scoped library paths (e.g. /Users/{id}/Items, /Users/{id}/Views).
		if len(parts) >= 3 {
			return metadata(method), true
		}
		return Decision{}, false
	case "system":
		// Non-public system info and similar metadata reads.
		if len(parts) >= 2 && parts[1] == "info" {
			return metadata(method), true
		}
		return Decision{}, false
	default:
		return Decision{}, false
	}
}

func personal() Decision {
	return Decision{Ownership: LocalPersonal, Operation: OperationPersonal, MethodAllowed: true}
}

func media(method string) Decision {
	return methodDecision(method, MediaProxy, OperationMediaProxy, "GET", "HEAD")
}

func metadata(method string) Decision {
	return methodDecision(method, MetadataProxy, OperationMetadataProxy, "GET", "HEAD")
}

func methodDecision(method string, ownership Ownership, operation Operation, allowed ...string) Decision {
	allow := strings.Join(allowed, ", ")
	ok := false
	for _, a := range allowed {
		if method == a {
			ok = true
			break
		}
	}
	return Decision{
		Ownership:     ownership,
		Operation:     operation,
		MethodAllowed: ok,
		Allow:         allow,
	}
}

// normalizePath accepts path-only input. It does not strip query/fragment markers
// or trim surrounding whitespace; those remain path data when present.
func normalizePath(relPath string) string {
	path := relPath
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	return path
}

func pathParts(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	raw := strings.Split(path, "/")
	parts := make([]string, len(raw))
	for i, p := range raw {
		parts[i] = strings.ToLower(p)
	}
	return parts
}
