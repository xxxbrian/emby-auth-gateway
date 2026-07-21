package routeclass

import "strings"

// InventoryRule is one ordinary-client allow template with exact successful methods
// and curated contract metadata. It is the single source of truth for Classify
// allow-matching and corpus drift tests. Production never parses JSON corpus files.
type InventoryRule struct {
	// Methods are the exact successful HTTP methods for PathTemplate.
	Methods []string
	// PathTemplate uses Emby-relative form with brace placeholders (no /emby mount).
	PathTemplate string
	Ownership    Ownership
	Operation    Operation
	// AuthMode, Projection, and Egress are curated contract fields compared by
	// drift tests. Classify uses Ownership/Operation/Methods only.
	AuthMode   string
	Projection string
	Egress     string
}

// Inventory returns a deep copy of the authoritative ordinary-client allow
// inventory. Callers may mutate the returned slice and nested Methods slices
// without affecting package authorization state used by Classify.
// DeniedSession families and Unclassified defaults are not listed here.
func Inventory() []InventoryRule {
	out := make([]InventoryRule, len(inventoryRules))
	for i, rule := range inventoryRules {
		out[i] = rule
		if rule.Methods != nil {
			out[i].Methods = append([]string(nil), rule.Methods...)
		}
	}
	return out
}

// inventoryRules is ordered so more specific static templates precede dynamic ones
// (e.g. System/Info/Public before System/Info, Users/Public before Users/{UserId},
// ActiveEncodings before Videos/{ItemId}/stream, Resume before Users/{UserId}/Items/{ItemId}).
var inventoryRules = []InventoryRule{
	// --- LocalPublic ---
	{[]string{"POST"}, "/Users/AuthenticateByName", LocalPublic, OperationAuthenticate, "public", "none", "none"},
	{[]string{"GET"}, "/System/Info/Public", LocalPublic, OperationPublicSystemInfo, "public", "none", "none"},
	{[]string{"GET", "POST"}, "/System/Ping", LocalPublic, OperationPing, "public", "none", "none"},
	{[]string{"GET"}, "/Users/Public", LocalPublic, OperationPublicUsers, "public", "none", "none"},
	{[]string{"GET"}, "/Branding/Configuration", LocalPublic, OperationBrandingConfiguration, "public", "none", "none"},
	{[]string{"GET"}, "/Branding/Css.css", LocalPublic, OperationBrandingCSS, "public", "none", "none"},

	// --- LocalSession (exact local + targeted control) ---
	{[]string{"GET"}, "/embywebsocket", LocalSession, OperationWebSocket, "session", "none", "none"},
	{[]string{"GET"}, "/Sessions", LocalSession, OperationSessionList, "session", "none", "none"},
	{[]string{"POST"}, "/Sessions/Logout", LocalSession, OperationLogout, "session", "none", "none"},
	{[]string{"POST"}, "/Sessions/Playing", LocalSession, OperationPlaybackReport, "session", "none", "none"},
	{[]string{"POST"}, "/Sessions/Playing/Progress", LocalSession, OperationPlaybackReport, "session", "none", "none"},
	{[]string{"POST"}, "/Sessions/Playing/Stopped", LocalSession, OperationPlaybackReport, "session", "none", "none"},
	{[]string{"POST"}, "/Sessions/Playing/Ping", LocalSession, OperationPlaybackPing, "session", "none", "none"},
	{[]string{"POST"}, "/Sessions/Capabilities", LocalSession, OperationCapabilities, "session", "none", "none"},
	{[]string{"POST"}, "/Sessions/Capabilities/Full", LocalSession, OperationCapabilities, "session", "none", "none"},
	{[]string{"POST"}, "/Sessions/{Id}/Playing", LocalSession, OperationSessionPlay, "session", "none", "none"},
	{[]string{"POST"}, "/Sessions/{Id}/Playing/{Command}", LocalSession, OperationSessionPlaystate, "session", "none", "none"},
	{[]string{"POST"}, "/Sessions/{Id}/Command", LocalSession, OperationSessionGeneralCommand, "session", "none", "none"},
	{[]string{"POST"}, "/Sessions/{Id}/Command/{Command}", LocalSession, OperationSessionGeneralCommand, "session", "none", "none"},

	// --- LocalPersonal (exact methods from handlers) ---
	{[]string{"GET"}, "/Users/{UserId}", LocalPersonal, OperationCurrentUser, "session", "none", "none"},
	{[]string{"GET", "POST"}, "/DisplayPreferences/{Id}", LocalPersonal, OperationPersonal, "session", "none", "none"},
	{[]string{"GET"}, "/Shows/NextUp", LocalPersonal, OperationPersonal, "session", "BaseItemEnvelope", "none"},
	{[]string{"GET"}, "/Users/{UserId}/Items/Resume", LocalPersonal, OperationPersonal, "session", "BaseItemEnvelope", "none"},
	{[]string{"GET"}, "/Users/{UserId}/Items/Latest", LocalPersonal, OperationPersonal, "session", "BaseItemArray", "none"},
	{[]string{"POST", "DELETE"}, "/Users/{UserId}/PlayedItems/{ItemId}", LocalPersonal, OperationPersonal, "session", "none", "none"},
	{[]string{"POST", "DELETE"}, "/Users/{UserId}/FavoriteItems/{ItemId}", LocalPersonal, OperationPersonal, "session", "none", "none"},
	{[]string{"POST", "DELETE"}, "/Users/{UserId}/Items/{ItemId}/Rating", LocalPersonal, OperationPersonal, "session", "none", "none"},
	{[]string{"POST"}, "/Users/{UserId}/Items/{ItemId}/UserData", LocalPersonal, OperationPersonal, "session", "none", "none"},

	// --- Negotiation (MediaProxy) ---
	{[]string{"GET", "POST"}, "/Items/{ItemId}/PlaybackInfo", MediaProxy, OperationPlaybackInfo, "session", "PlaybackInfo", "negotiation"},
	{[]string{"POST"}, "/LiveStreams/Open", MediaProxy, OperationLiveStreamOpen, "session", "LiveStreamResponse", "negotiation"},
	{[]string{"POST"}, "/LiveStreams/MediaInfo", MediaProxy, OperationLiveStreamMediaInfo, "session", "MediaSource", "negotiation"},
	{[]string{"POST"}, "/LiveStreams/Close", MediaProxy, OperationLiveStreamClose, "session", "none", "negotiation"},
	{[]string{"DELETE"}, "/Videos/ActiveEncodings", MediaProxy, OperationActiveEncodingsDelete, "session", "none", "negotiation"},
	{[]string{"POST"}, "/Videos/ActiveEncodings/Delete", MediaProxy, OperationActiveEncodingsDeleteCompat, "session", "none", "negotiation"},

	// --- MetadataProxy ---
	{[]string{"GET", "HEAD"}, "/System/Info", MetadataProxy, OperationMetadataProxy, "session", "SystemInfo", "metadata"},
	{[]string{"GET", "HEAD"}, "/System/Endpoint", MetadataProxy, OperationMetadataProxy, "session", "opaque", "metadata"},
	{[]string{"GET", "HEAD"}, "/Users/{UserId}/Views", MetadataProxy, OperationMetadataProxy, "session", "BaseItemEnvelope", "metadata"},
	{[]string{"GET", "HEAD"}, "/Users/{UserId}/Items", MetadataProxy, OperationMetadataProxy, "session", "BaseItemEnvelope", "metadata"},
	{[]string{"GET", "HEAD"}, "/Users/{UserId}/Items/{ItemId}", MetadataProxy, OperationMetadataProxy, "session", "BaseItem", "metadata"},
	{[]string{"GET", "HEAD"}, "/Users/{UserId}/Items/{ItemId}/SpecialFeatures", MetadataProxy, OperationMetadataProxy, "session", "BaseItemArray", "metadata"},
	{[]string{"GET", "HEAD"}, "/Items/{ItemId}/Similar", MetadataProxy, OperationMetadataProxy, "session", "BaseItemEnvelope", "metadata"},
	{[]string{"GET", "HEAD"}, "/Items/{ItemId}/ThemeMedia", MetadataProxy, OperationMetadataProxy, "session", "AllThemeMedia", "metadata"},

	// --- MediaProxy binary / stream / HLS / subtitle ---
	// Binary item images: tokenless anonymous, resource-cookie, or session auth.
	{[]string{"GET", "HEAD"}, "/Items/{ItemId}/Images/{ImageType}", MediaProxy, OperationMediaProxy, "anonymous-or-resource-cookie-or-session", "opaque", "media"},
	{[]string{"GET", "HEAD"}, "/Items/{ItemId}/Images/{ImageType}/{Index}", MediaProxy, OperationMediaProxy, "anonymous-or-resource-cookie-or-session", "opaque", "media"},
	{[]string{"GET", "HEAD"}, "/Items/{ItemId}/Download", MediaProxy, OperationMediaProxy, "resource-cookie-or-session", "opaque", "media"},
	{[]string{"GET", "HEAD"}, "/Videos/{ItemId}/stream", MediaProxy, OperationMediaProxy, "resource-cookie-or-session", "opaque", "media"},
	{[]string{"GET", "HEAD"}, "/Videos/{ItemId}/stream.{Container}", MediaProxy, OperationMediaProxy, "resource-cookie-or-session", "opaque", "media"},
	{[]string{"GET", "HEAD"}, "/Videos/{ItemId}/master.m3u8", MediaProxy, OperationMediaProxy, "resource-cookie-or-session", "opaque", "media"},
	{[]string{"GET", "HEAD"}, "/Videos/{ItemId}/main.m3u8", MediaProxy, OperationMediaProxy, "resource-cookie-or-session", "opaque", "media"},
	{[]string{"GET", "HEAD"}, "/Videos/{ItemId}/hls/{SegmentId}.{SegmentContainer}", MediaProxy, OperationMediaProxy, "resource-cookie-or-session", "opaque", "media"},
	{[]string{"GET", "HEAD"}, "/Videos/{ItemId}/hls/{PlaylistId}/{SegmentId}.{SegmentContainer}", MediaProxy, OperationMediaProxy, "resource-cookie-or-session", "opaque", "media"},
	{[]string{"GET", "HEAD"}, "/Videos/{ItemId}/hls1/{SegmentId}.{SegmentContainer}", MediaProxy, OperationMediaProxy, "resource-cookie-or-session", "opaque", "media"},
	{[]string{"GET", "HEAD"}, "/Videos/{ItemId}/hls1/{PlaylistId}/{SegmentId}.{SegmentContainer}", MediaProxy, OperationMediaProxy, "resource-cookie-or-session", "opaque", "media"},
	{[]string{"GET", "HEAD"}, "/Videos/{ItemId}/{MediaSourceId}/Subtitles/{Index}/Stream.{Format}", MediaProxy, OperationMediaProxy, "resource-cookie-or-session", "opaque", "media"},
	{[]string{"GET", "HEAD"}, "/Videos/{ItemId}/{MediaSourceId}/Subtitles/{Index}/{StartPositionTicks}/Stream.{Format}", MediaProxy, OperationMediaProxy, "resource-cookie-or-session", "opaque", "media"},
	{[]string{"GET", "HEAD"}, "/Items/{ItemId}/{MediaSourceId}/Subtitles/{Index}/Stream.{Format}", MediaProxy, OperationMediaProxy, "resource-cookie-or-session", "opaque", "media"},
	{[]string{"GET", "HEAD"}, "/Items/{ItemId}/{MediaSourceId}/Subtitles/{Index}/{StartPositionTicks}/Stream.{Format}", MediaProxy, OperationMediaProxy, "resource-cookie-or-session", "opaque", "media"},
	{[]string{"GET", "HEAD"}, "/Audio/{ItemId}/stream", MediaProxy, OperationMediaProxy, "resource-cookie-or-session", "opaque", "media"},
	{[]string{"GET", "HEAD"}, "/Audio/{ItemId}/stream.{Container}", MediaProxy, OperationMediaProxy, "resource-cookie-or-session", "opaque", "media"},
}

// matchInventory returns a Decision when parts match an inventory template.
// On template match with a disallowed method, returns MethodAllowed=false with
// the rule's ownership/operation (known-route 405), not Unclassified.
func matchInventory(method string, parts []string) (Decision, bool) {
	for _, rule := range inventoryRules {
		if !matchTemplate(parts, rule.PathTemplate) {
			continue
		}
		return methodDecision(method, rule.Ownership, rule.Operation, rule.Methods...), true
	}
	return Decision{}, false
}

func matchTemplate(parts []string, template string) bool {
	tmpl := strings.Trim(template, "/")
	if tmpl == "" {
		return len(parts) == 0
	}
	segs := strings.Split(tmpl, "/")
	if len(segs) != len(parts) {
		return false
	}
	for i, seg := range segs {
		got := parts[i]
		// Compound allowlisted forms before plain {Name} placeholders.
		// stream.{Container}
		if strings.HasPrefix(strings.ToLower(seg), "stream.{") && strings.HasSuffix(seg, "}") {
			// Distinguish subtitle Stream.{Format} (capital Stream in template) via format allowlist
			// vs media stream.{Container}. Both use stream. prefix on the concrete segment.
			if strings.HasPrefix(seg, "Stream.") {
				if !isAllowlistedSubtitleStreamFile(got) {
					return false
				}
			} else if !isAllowlistedStreamContainer(got) {
				return false
			}
			continue
		}
		// {SegmentId}.{SegmentContainer} (contains }.{ between braces)
		if strings.HasPrefix(seg, "{") && strings.Contains(seg, "}.{") && strings.HasSuffix(seg, "}") {
			if !isAllowlistedHLSSegmentFile(got) {
				return false
			}
			continue
		}
		// Plain single placeholder: entire segment is {Name}.
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") && !strings.Contains(seg[1:len(seg)-1], "{") && !strings.Contains(seg[1:len(seg)-1], "}") {
			if got == "" {
				return false
			}
			name := seg[1 : len(seg)-1]
			switch name {
			case "Index", "StartPositionTicks":
				if !isDecimalSegment(got) {
					return false
				}
			}
			continue
		}
		// Static segment (case-insensitive; parts already lowercased).
		if strings.ToLower(seg) != got {
			return false
		}
	}
	return true
}

// OwnershipName returns the curated string form of o.
func OwnershipName(o Ownership) string {
	switch o {
	case LocalPublic:
		return "LocalPublic"
	case LocalPersonal:
		return "LocalPersonal"
	case LocalSession:
		return "LocalSession"
	case MetadataProxy:
		return "MetadataProxy"
	case MediaProxy:
		return "MediaProxy"
	case DeniedSession:
		return "DeniedSession"
	case Unclassified:
		return "Unclassified"
	default:
		return "Unknown"
	}
}

// OperationName returns the curated short name for op.
func OperationName(op Operation) string {
	switch op {
	case OperationAuthenticate:
		return "Authenticate"
	case OperationPublicSystemInfo:
		return "PublicSystemInfo"
	case OperationPing:
		return "Ping"
	case OperationLogout:
		return "Logout"
	case OperationPublicUsers:
		return "PublicUsers"
	case OperationCurrentUser:
		return "CurrentUser"
	case OperationBrandingConfiguration:
		return "BrandingConfiguration"
	case OperationBrandingCSS:
		return "BrandingCSS"
	case OperationPersonal:
		return "Personal"
	case OperationSessionList:
		return "SessionList"
	case OperationPlaybackReport:
		return "PlaybackReport"
	case OperationPlaybackPing:
		return "PlaybackPing"
	case OperationCapabilities:
		return "Capabilities"
	case OperationDeniedSession:
		return "DeniedSession"
	case OperationMetadataProxy:
		return "MetadataProxy"
	case OperationMediaProxy:
		return "MediaProxy"
	case OperationPlaybackInfo:
		return "PlaybackInfo"
	case OperationLiveStreamOpen:
		return "LiveStreamOpen"
	case OperationLiveStreamMediaInfo:
		return "LiveStreamMediaInfo"
	case OperationLiveStreamClose:
		return "LiveStreamClose"
	case OperationActiveEncodingsDelete:
		return "ActiveEncodingsDelete"
	case OperationActiveEncodingsDeleteCompat:
		return "ActiveEncodingsDeleteCompat"
	case OperationWebSocket:
		return "WebSocket"
	case OperationSessionGeneralCommand:
		return "SessionGeneralCommand"
	case OperationSessionPlay:
		return "SessionPlay"
	case OperationSessionPlaystate:
		return "SessionPlaystate"
	case OperationUnclassified:
		return "Unclassified"
	default:
		return "Unknown"
	}
}
