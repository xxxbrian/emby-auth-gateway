// Package routeclass classifies Emby-relative HTTP routes by architectural ownership.
//
// It is a stdlib-only leaf package shared by gateway dispatch and observe telemetry.
// Path matching is case-insensitive and never embeds raw user IDs in outputs.
//
// Callers must pass a path-only value (for example net/http Request.URL.Path or the
// gateway Emby-relative path). Never pass RawQuery or a full request URI; query and
// fragment characters that appear in the path string are treated as path data.
//
// Phase 8 classifier policy: unknown method/path pairs are Unclassified with methods
// denied. Ordinary-client allow routes come from the declarative Inventory() table
// (exact templates and exact successful methods). DeniedSession families remain
// explicit. There is no Legacy ownership or fallback proxy path.
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
	// Unclassified is the default-deny ownership for method/path pairs outside the
	// curated and gateway-local template set.
	Unclassified
)

// Operation identifies the recognized handler family for a decision.
type Operation uint8

const (
	OperationAuthenticate Operation = iota + 1
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
	// OperationUnclassified is the default-deny operation for unmatched routes.
	OperationUnclassified
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
//
// Unmatched routes return Unclassified with MethodAllowed false. Matching never uses
// prefix, suffix, or substring wildcards over media/metadata trees.
func Classify(method, relPath string) Decision {
	method = strings.ToUpper(strings.TrimSpace(method))
	parts := pathParts(normalizePath(relPath))

	// Ordinary-client allow inventory (exact templates + exact methods).
	if d, ok := matchInventory(method, parts); ok {
		return d
	}
	// Session denial families (not ordinary-client allow rules).
	if d, ok := classifySessionsDenial(method, parts); ok {
		return d
	}
	return unclassified()
}

func unclassified() Decision {
	return Decision{
		Ownership:     Unclassified,
		Operation:     OperationUnclassified,
		MethodAllowed: false,
		Allow:         "",
	}
}

func classifySessionsDenial(method string, parts []string) (Decision, bool) {
	if len(parts) == 0 || parts[0] != "sessions" {
		return Decision{}, false
	}
	// Exact allow routes are in Inventory; only denials remain here.
	switch {
	case len(parts) == 2 && parts[1] == "playqueue":
		return methodDecision(method, DeniedSession, OperationDeniedSession, "GET"), true
	}

	if d, ok := classifyTargetedSessionDenial(method, parts); ok {
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

func classifyTargetedSessionDenial(method string, parts []string) (Decision, bool) {
	if len(parts) < 3 {
		return Decision{}, false
	}
	rest := parts[2:]
	switch {
	case rest[0] == "system" && len(rest) == 2:
		return methodDecision(method, DeniedSession, OperationDeniedSession, "POST"), true
	case rest[0] == "message" && len(rest) == 1:
		return methodDecision(method, DeniedSession, OperationDeniedSession, "POST"), true
	case rest[0] == "viewing" && len(rest) == 1:
		return methodDecision(method, DeniedSession, OperationDeniedSession, "POST"), true
	case rest[0] == "users" && len(rest) == 2:
		return methodDecision(method, DeniedSession, OperationDeniedSession, "POST", "DELETE"), true
	case rest[0] == "users" && len(rest) == 3 && rest[2] == "delete":
		return methodDecision(method, DeniedSession, OperationDeniedSession, "POST"), true
	default:
		return Decision{}, false
	}
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

func isDecimalSegment(seg string) bool {
	if seg == "" {
		return false
	}
	for _, r := range seg {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isAllowlistedStreamContainer(seg string) bool {
	const prefix = "stream."
	if !strings.HasPrefix(seg, prefix) {
		return false
	}
	return mediaStreamContainers[seg[len(prefix):]]
}

func isAllowlistedHLSSegmentFile(seg string) bool {
	dot := strings.LastIndexByte(seg, '.')
	if dot <= 0 || dot == len(seg)-1 {
		return false
	}
	return hlsSegmentContainers[seg[dot+1:]]
}

func isAllowlistedSubtitleStreamFile(seg string) bool {
	const prefix = "stream."
	if !strings.HasPrefix(seg, prefix) {
		return false
	}
	return subtitleStreamFormats[seg[len(prefix):]]
}

// Finite allowlists — explicit admission, not extension wildcards.
var mediaStreamContainers = map[string]bool{
	"ts": true, "mp4": true, "mkv": true, "webm": true, "m4v": true, "mov": true,
	"flv": true, "avi": true, "wmv": true, "asf": true, "mpg": true, "mpeg": true,
	"m2ts": true, "mts": true, "3gp": true, "ogg": true, "ogv": true,
	"mp3": true, "aac": true, "m4a": true, "flac": true, "wma": true, "wav": true,
	"opus": true, "webma": true, "oga": true,
}

var hlsSegmentContainers = map[string]bool{
	"ts": true, "m4s": true, "mp4": true, "aac": true, "vtt": true,
}

var subtitleStreamFormats = map[string]bool{
	"vtt": true, "srt": true, "ass": true, "ssa": true, "subrip": true,
	"ttml": true, "dvbsub": true, "pgs": true, "pgssub": true, "dvdsub": true,
	"sub": true, "smi": true, "sami": true, "json": true,
}
