// Package subtitleindex reads indexed Matroska text subtitles without scanning
// media payloads. It has no background work, shared state, or playback side effects.
package subtitleindex

import (
	"context"
	"errors"
	"io"
)

var (
	ErrUnsupported = errors.New("subtitle extraction is unsupported")
	ErrLimit       = errors.New("subtitle extraction resource limit exceeded")
	ErrSource      = errors.New("subtitle source read failed")
	ErrIndex       = errors.New("subtitle index is missing or invalid")
	// ErrEmpty means every successfully validated indexed packet was empty. It
	// does not establish that an unindexed packet cannot exist elsewhere.
	ErrEmpty = errors.New("indexed subtitle packets are empty")
)

type Source struct {
	Size      int64
	Container string
	// Open must return exactly the requested source range. Implementations must
	// honor cancellation and validate the origin's range and source identity.
	Open func(context.Context, int64, int64) (io.ReadCloser, error)
}

type Limits struct {
	// Zero selects a bounded default. Bytes and requests include reads served
	// by Source.Open's cache; origin-only accounting belongs to that adapter.
	MaxReadBytes   int64
	MaxRequests    int
	MaxOutputBytes int64
	MaxCues        int
}

// Track.Index is the zero-based media stream index, not a Matroska TrackNumber.
// Codec and optional Language are checked against the container before reading
// any subtitle packets. Name is descriptive and does not establish identity.
type Track struct {
	Index    int
	Codec    string
	Language string
	Name     string
}

type Result struct {
	Data      []byte
	Format    string
	Cues      int
	ReadBytes int64 // bytes received from Source.Open, including cached reads
	Requests  int   // Source.Open calls, including cached reads
}

func (l Limits) normalized() (Limits, error) {
	if l.MaxReadBytes < 0 || l.MaxRequests < 0 || l.MaxOutputBytes < 0 || l.MaxCues < 0 {
		return Limits{}, ErrLimit
	}
	if l.MaxReadBytes == 0 {
		l.MaxReadBytes = 64 << 20
	}
	if l.MaxRequests == 0 {
		l.MaxRequests = 2048
	}
	if l.MaxOutputBytes == 0 {
		l.MaxOutputBytes = 8 << 20
	}
	if l.MaxCues == 0 {
		l.MaxCues = 20000
	}
	// Hard bounds also apply to callers that accidentally supply huge limits.
	l.MaxReadBytes = min(l.MaxReadBytes, 128<<20)
	l.MaxRequests = min(l.MaxRequests, 8192)
	l.MaxOutputBytes = min(l.MaxOutputBytes, 16<<20)
	l.MaxCues = min(l.MaxCues, 100000)
	return l, nil
}
