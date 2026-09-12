// Package subtitles provides bounded, optional subtitle preparation for an
// already authenticated first-party Web playback. Native requests never enter it.
package subtitles

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrUnavailable = errors.New("subtitle unavailable")
	ErrCapacity    = errors.New("subtitle capacity exhausted")
	ErrClosed      = errors.New("subtitle manager closed")
)

type Config struct {
	Dir              string
	CacheBytes       int64
	SourceCacheBytes int64
	Workers          int
	PrepareTimeout   time.Duration
	WorkTimeout      time.Duration
	MaxReadBytes     int64
	MaxRequests      int
	BootID           string
}

type Track struct {
	Index    int
	Codec    string
	Language string
	Name     string
	External bool
}

type Input struct {
	Owner      string
	PlaybackID string
	ItemID     string
	SourceID   string
	SourceKey  string
	SourceName string
	Size       int64
	Container  string
	Tracks     []Track
	Open       func(context.Context, int64, int64) (io.ReadCloser, error)
	Validate   func(context.Context) (version string, strong bool, err error)
	Fetch      func(context.Context, Track) (data []byte, format string, err error)
	Valid      func(context.Context) bool
	// ValidTrack rechecks current delivery policy without probing the origin.
	// It is called for ready metadata and cached grants, outside manager locks.
	ValidTrack func(context.Context, Track) bool
}

type Selection struct {
	Playback bool
	// Recover allows small indexed preparation for an explicitly selected
	// first-party Web detail preview. It never permits a source scan.
	Recover bool
	// Probe permits bounded upstream subtitle requests. False only projects
	// already-ready artifacts, as required for catalog/list responses.
	Probe bool
	Index *int
}

type Ready struct {
	Index  int
	ID     string
	Format string
}

type TrackView struct {
	Index      int    `json:"index"`
	Language   string `json:"language"`
	Name       string `json:"name"`
	State      string `json:"state"`
	Format     string `json:"format,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Persistent bool   `json:"persistent"`
	Bytes      int64  `json:"bytes"`
	Cues       int    `json:"cues"`
	ReadBytes  int64  `json:"read_bytes"`
	Requests   int    `json:"requests"`
	Reason     string `json:"reason,omitempty"`
}

type JobView struct {
	ID         string      `json:"id"`
	ItemID     string      `json:"item_id"`
	SourceID   string      `json:"source_id"`
	SourceName string      `json:"source_name"`
	SourceSize int64       `json:"source_size"`
	State      string      `json:"state"`
	CreatedAt  time.Time   `json:"created_at"`
	UpdatedAt  time.Time   `json:"updated_at"`
	Viewers    int         `json:"viewers"`
	Tracks     []TrackView `json:"tracks"`
}

type Snapshot struct {
	Enabled           bool      `json:"enabled"`
	Reason            string    `json:"reason,omitempty"`
	BootID            string    `json:"boot_id"`
	Workers           int       `json:"workers"`
	Active            int       `json:"active"`
	CacheBytes        int64     `json:"cache_bytes"`
	CacheBudget       int64     `json:"cache_budget"`
	SourceCacheBytes  int64     `json:"source_cache_bytes"`
	SourceCacheBudget int64     `json:"source_cache_budget"`
	SourceReadBytes   int64     `json:"source_read_bytes"`
	SourceHitBytes    int64     `json:"source_hit_bytes"`
	Grants            int       `json:"grants"`
	Jobs              []JobView `json:"jobs"`
}
