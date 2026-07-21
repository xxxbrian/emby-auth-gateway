package gateway

// responseProjectionKind declares the successful response shape selected by
// dispatch. The zero value is deliberately invalid so callers cannot obtain
// array semantics by omission or inference.
type responseProjectionKind uint8

const (
	responseProjectionOpaque responseProjectionKind = iota + 1
	responseProjectionBaseItem
	responseProjectionBaseItemEnvelope
	responseProjectionBaseItemArray
	responseProjectionSystemInfo
	responseProjectionPlaybackInfo
	responseProjectionBaseItemEnvelopeArray
	responseProjectionAllThemeMedia
	responseProjectionLiveStreamResponse
	responseProjectionMediaSource
)

type responseProjection struct {
	kind responseProjectionKind
}

func newResponseProjection(kind responseProjectionKind) responseProjection {
	return responseProjection{kind: kind}
}

func (p responseProjection) valid() bool {
	return p.kind >= responseProjectionOpaque && p.kind <= responseProjectionMediaSource
}

func (p responseProjection) declaredBaseItemArray() bool {
	return p.kind == responseProjectionBaseItemArray
}
