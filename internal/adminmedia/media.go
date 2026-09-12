// Package adminmedia provides bounded, read-only catalogue views for admins.
// Its transport contract cannot mutate Gateway personal state or expose auth.
package adminmedia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	MaxBatch           = 50
	maxCacheEntries    = 1000
	maxCacheBytes      = 8 << 20
	maxImageEntries    = 64
	maxImageCacheBytes = 16 << 20
	requestTimeout     = 3 * time.Second
)

var ErrInvalid = errors.New("invalid media request")

// Reader is implemented by the gateway's narrow, authenticated GET adapter.
type Reader interface {
	AdminMediaSourceRef(context.Context) (string, error)
	AdminMediaItems(context.Context, string, []string, bool) ([]byte, int, error)
	AdminMediaImage(context.Context, string, string, string, string) ([]byte, string, int, error)
}

type Stream struct {
	Type       string `json:"type"`
	Codec      string `json:"codec,omitempty"`
	Language   string `json:"language,omitempty"`
	Title      string `json:"title,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	Channels   int    `json:"channels,omitempty"`
	IsDefault  bool   `json:"is_default,omitempty"`
	IsExternal bool   `json:"is_external,omitempty"`
}

// Item deliberately excludes UserData, paths, external URLs and MediaSources.
type Item struct {
	ID              string     `json:"id"`
	SourceRef       string     `json:"source_ref"`
	Status          string     `json:"status"`
	Stale           bool       `json:"stale,omitempty"`
	FetchedAt       *time.Time `json:"fetched_at,omitempty"`
	Type            string     `json:"type,omitempty"`
	Name            string     `json:"name,omitempty"`
	SeriesName      string     `json:"series_name,omitempty"`
	SeriesID        string     `json:"series_id,omitempty"`
	SeasonNumber    *int       `json:"season_number,omitempty"`
	EpisodeNumber   *int       `json:"episode_number,omitempty"`
	ProductionYear  *int       `json:"production_year,omitempty"`
	RuntimeTicks    *int64     `json:"runtime_ticks,omitempty"`
	Image           string     `json:"image,omitempty"`
	Overview        string     `json:"overview,omitempty"`
	Genres          []string   `json:"genres,omitempty"`
	CommunityRating *float64   `json:"community_rating,omitempty"`
	OfficialRating  string     `json:"official_rating,omitempty"`
	Container       string     `json:"container,omitempty"`
	MediaStreams    []Stream   `json:"media_streams,omitempty"`
}

type cacheEntry struct {
	item          Item
	expires, used time.Time
	bytes         int
}
type imageEntry struct {
	data          []byte
	contentType   string
	expires, used time.Time
}

type Service struct {
	reader                    Reader
	mu                        sync.Mutex
	cache                     map[string]cacheEntry
	pending                   map[string]chan struct{}
	images                    map[string]imageEntry
	cacheBytes, imageBytes    int
	activeSource              string
	metadataSlots, imageSlots chan struct{}
	imageAdmissions           chan struct{}
	imageFlights              singleflight.Group
	now                       func() time.Time
}

func New(reader Reader) *Service {
	return &Service{reader: reader, cache: map[string]cacheEntry{}, pending: map[string]chan struct{}{}, images: map[string]imageEntry{}, metadataSlots: make(chan struct{}, 4), imageSlots: make(chan struct{}, 4), imageAdmissions: make(chan struct{}, 32), now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) SourceRef(ctx context.Context) (string, error) {
	if s == nil || s.reader == nil {
		return "", errors.New("media source unavailable")
	}
	ref, err := s.reader.AdminMediaSourceRef(ctx)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	if ref != s.activeSource {
		s.activeSource = ref
		s.cache = map[string]cacheEntry{}
		s.images = map[string]imageEntry{}
		s.cacheBytes, s.imageBytes = 0, 0
	}
	s.mu.Unlock()
	return ref, nil
}

func ValidID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func ValidateIDs(ids []string) ([]string, error) {
	if len(ids) == 0 || len(ids) > MaxBatch {
		return nil, ErrInvalid
	}
	unique := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if !ValidID(id) {
			return nil, ErrInvalid
		}
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	return unique, nil
}

func (s *Service) Items(ctx context.Context, source string, ids []string, detail bool) ([]Item, error) {
	ids, err := ValidateIDs(ids)
	if err != nil || detail && len(ids) != 1 || source != "" && !ValidID(source) {
		return nil, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	current, err := s.SourceRef(ctx)
	if err != nil {
		return statuses(ids, source, "unavailable"), nil
	}
	if source == "" || current != source {
		return statuses(ids, source, "source_changed"), nil
	}
	keys := make([]string, len(ids))
	needed := make([]string, 0, len(ids))
	waits := make([]chan struct{}, 0, len(ids))
	now := s.now()
	s.mu.Lock()
	for i, id := range ids {
		key := fmt.Sprintf("%s:%t:%s", source, detail, id)
		keys[i] = key
		if entry, ok := s.cache[key]; ok && now.Before(entry.expires) {
			entry.used = now
			s.cache[key] = entry
			continue
		}
		if wait, ok := s.pending[key]; ok {
			waits = append(waits, wait)
			continue
		}
		// Bound pending work as well as successful cache entries.
		if len(s.pending) >= maxCacheEntries {
			continue
		}
		s.pending[key] = make(chan struct{})
		needed = append(needed, id)
	}
	s.mu.Unlock()
	if len(needed) > 0 {
		s.fetch(ctx, source, needed, detail)
	}
	for _, wait := range waits {
		select {
		case <-wait:
		case <-ctx.Done():
		}
	}
	result := statuses(ids, source, "unavailable")
	s.mu.Lock()
	for i, key := range keys {
		if entry, ok := s.cache[key]; ok {
			result[i] = entry.item
			if !s.now().Before(entry.expires) && result[i].Status == "available" {
				result[i].Stale = true
			}
		}
	}
	s.mu.Unlock()
	// Reconfiguration cannot turn an in-flight read of A into a view of B.
	current, err = s.SourceRef(ctx)
	if err == nil && current != source {
		return statuses(ids, source, "source_changed"), nil
	}
	if err != nil && ctx.Err() == nil {
		return statuses(ids, source, "unavailable"), nil
	}
	return result, nil
}

func statuses(ids []string, source, status string) []Item {
	items := make([]Item, len(ids))
	for i, id := range ids {
		items[i] = Item{ID: id, SourceRef: source, Status: status}
	}
	return items
}

func (s *Service) fetch(ctx context.Context, source string, ids []string, detail bool) {
	items := statuses(ids, source, "unavailable")
	select {
	case s.metadataSlots <- struct{}{}:
		data, status, err := s.reader.AdminMediaItems(ctx, source, ids, detail)
		<-s.metadataSlots
		if status == http.StatusConflict {
			items = statuses(ids, source, "source_changed")
		} else if err == nil && status == http.StatusNotFound {
			items = statuses(ids, source, "missing")
		} else if err == nil && status == http.StatusOK && len(data) <= 2<<20 {
			if decoded, ok := decode(data, source, ids, detail, s.now()); ok {
				items = decoded
			}
		}
	case <-ctx.Done():
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, id := range ids {
		key := fmt.Sprintf("%s:%t:%s", source, detail, id)
		item := items[i]
		if s.activeSource != source {
			if ch, ok := s.pending[key]; ok {
				close(ch)
				delete(s.pending, key)
			}
			continue
		}
		ttl := 2 * time.Second
		if item.Status == "available" {
			ttl = 10 * time.Minute
		} else if item.Status == "missing" {
			ttl = 30 * time.Second
		}
		if old, ok := s.cache[key]; ok && old.item.Status == "available" && item.Status == "unavailable" && old.item.FetchedAt != nil && now.Sub(*old.item.FetchedAt) < time.Hour {
			item = old.item
			item.Stale = true
		}
		encoded, _ := json.Marshal(item)
		if old, ok := s.cache[key]; ok {
			s.cacheBytes -= old.bytes
		}
		s.cache[key] = cacheEntry{item: item, expires: now.Add(ttl), used: now, bytes: len(encoded)}
		s.cacheBytes += len(encoded)
		if ch, ok := s.pending[key]; ok {
			close(ch)
			delete(s.pending, key)
		}
	}
	s.trimCache()
}

func (s *Service) trimCache() {
	for len(s.cache) > maxCacheEntries || s.cacheBytes > maxCacheBytes {
		oldestKey := ""
		var oldest time.Time
		for key, entry := range s.cache {
			if oldestKey == "" || entry.used.Before(oldest) {
				oldestKey = key
				oldest = entry.used
			}
		}
		s.cacheBytes -= s.cache[oldestKey].bytes
		delete(s.cache, oldestKey)
	}
}

// Image returns only passive raster image bytes. Its independent bounded cache
// and concurrency pool never consume monitoring JSON request capacity.
func (s *Service) Image(ctx context.Context, source, id, kind, size string) ([]byte, string, int) {
	if !ValidID(id) || source != "" && !ValidID(source) || (kind != "Primary" && kind != "Thumb" && kind != "Backdrop") || (size != "small" && size != "large") {
		return nil, "", 400
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	current, err := s.SourceRef(ctx)
	if err != nil {
		return nil, "", 503
	}
	if source == "" || current != source {
		return nil, "", 409
	}
	key := strings.Join([]string{source, id, kind, size}, ":")
	s.mu.Lock()
	cached, ok := s.images[key]
	if ok && s.now().Before(cached.expires) {
		cached.used = s.now()
		s.images[key] = cached
		s.mu.Unlock()
		return cached.data, cached.contentType, 200
	}
	s.mu.Unlock()
	select {
	case s.imageAdmissions <- struct{}{}:
		defer func() { <-s.imageAdmissions }()
	default:
		return nil, "", http.StatusServiceUnavailable
	}
	ch := s.imageFlights.DoChan(key, func() (any, error) {
		select {
		case s.imageSlots <- struct{}{}:
			defer func() { <-s.imageSlots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		data, _, status, err := s.reader.AdminMediaImage(ctx, source, id, kind, size)
		if err != nil {
			return imageResult{status: 503}, nil
		}
		if status != 200 {
			return imageResult{status: status}, nil
		}
		if len(data) > 4<<20 {
			return imageResult{status: 503}, nil
		}
		kind := http.DetectContentType(data)
		if kind != "image/jpeg" && kind != "image/png" && kind != "image/webp" && kind != "image/gif" {
			return imageResult{status: 502}, nil
		}
		current, err := s.SourceRef(ctx)
		if err != nil {
			return imageResult{status: 503}, nil
		}
		if current != source {
			return imageResult{status: 409}, nil
		}
		now := s.now()
		s.mu.Lock()
		if s.activeSource != source {
			s.mu.Unlock()
			return imageResult{status: 409}, nil
		}
		if old, ok := s.images[key]; ok {
			s.imageBytes -= len(old.data)
		}
		s.images[key] = imageEntry{data: data, contentType: kind, expires: now.Add(10 * time.Minute), used: now}
		s.imageBytes += len(data)
		for len(s.images) > maxImageEntries || s.imageBytes > maxImageCacheBytes {
			oldKey := ""
			var oldTime time.Time
			for k, e := range s.images {
				if oldKey == "" || e.used.Before(oldTime) {
					oldKey = k
					oldTime = e.used
				}
			}
			s.imageBytes -= len(s.images[oldKey].data)
			delete(s.images, oldKey)
		}
		s.mu.Unlock()
		return imageResult{data: data, contentType: kind, status: 200}, nil
	})
	select {
	case <-ctx.Done():
		return nil, "", 503
	case result := <-ch:
		if result.Err != nil {
			return nil, "", 503
		}
		r := result.Val.(imageResult)
		return r.data, r.contentType, r.status
	}
}

type imageResult struct {
	data        []byte
	contentType string
	status      int
}

// Upstream JSON is decoded into a fixed projection; UserData and file paths
// cannot accidentally enter either the response or shared cache.
type upstreamItem struct {
	ID                    string            `json:"Id"`
	Type                  string            `json:"Type"`
	Name                  string            `json:"Name"`
	SeriesName            string            `json:"SeriesName"`
	SeriesID              string            `json:"SeriesId"`
	SeriesPrimaryImageTag string            `json:"SeriesPrimaryImageTag"`
	PrimaryImageItemID    string            `json:"PrimaryImageItemId"`
	PrimaryImageTag       string            `json:"PrimaryImageTag"`
	ParentThumbItemID     string            `json:"ParentThumbItemId"`
	ParentThumbImageTag   string            `json:"ParentThumbImageTag"`
	ImageTags             map[string]string `json:"ImageTags"`
	SeasonNumber          *int              `json:"ParentIndexNumber"`
	EpisodeNumber         *int              `json:"IndexNumber"`
	ProductionYear        *int              `json:"ProductionYear"`
	RuntimeTicks          *int64            `json:"RunTimeTicks"`
	Overview              string            `json:"Overview"`
	Genres                []string          `json:"Genres"`
	CommunityRating       *float64          `json:"CommunityRating"`
	OfficialRating        string            `json:"OfficialRating"`
	Container             string            `json:"Container"`
	MediaStreams          []struct {
		Type, Codec, Language, DisplayTitle string
		Width, Height, Channels             int
		IsDefault, IsExternal               bool
	} `json:"MediaStreams"`
}

func decode(data []byte, source string, ids []string, detail bool, now time.Time) ([]Item, bool) {
	var upstream []upstreamItem
	if detail {
		var one upstreamItem
		if json.Unmarshal(data, &one) != nil || one.ID != ids[0] {
			return nil, false
		}
		upstream = []upstreamItem{one}
	} else {
		var payload struct{ Items []upstreamItem }
		if json.Unmarshal(data, &payload) != nil || payload.Items == nil {
			return nil, false
		}
		upstream = payload.Items
	}
	if len(upstream) > MaxBatch {
		return nil, false
	}
	byID := map[string]upstreamItem{}
	for _, item := range upstream {
		byID[item.ID] = item
	}
	items := statuses(ids, source, "missing")
	for i, id := range ids {
		if raw, ok := byID[id]; ok {
			items[i] = project(raw, source, detail, now)
		}
	}
	return items, true
}

func project(raw upstreamItem, source string, detail bool, now time.Time) Item {
	item := Item{ID: raw.ID, SourceRef: source, Status: "available", FetchedAt: &now, Type: clean(raw.Type, 40), Name: clean(raw.Name, 500), SeriesName: clean(raw.SeriesName, 500), SeasonNumber: nonnegative(raw.SeasonNumber), EpisodeNumber: nonnegative(raw.EpisodeNumber), ProductionYear: nonnegative(raw.ProductionYear), RuntimeTicks: raw.RuntimeTicks}
	if ValidID(raw.SeriesID) {
		item.SeriesID = raw.SeriesID
	}
	if item.RuntimeTicks != nil && *item.RuntimeTicks < 0 {
		item.RuntimeTicks = nil
	}
	imageID, kind := "", ""
	for _, candidate := range []string{"Primary", "Thumb"} {
		if raw.ImageTags[candidate] != "" {
			imageID = raw.ID
			kind = candidate
			break
		}
	}
	if imageID == "" && raw.PrimaryImageTag != "" && ValidID(raw.PrimaryImageItemID) {
		imageID = raw.PrimaryImageItemID
		kind = "Primary"
	}
	if imageID == "" && raw.SeriesPrimaryImageTag != "" && ValidID(raw.SeriesID) {
		imageID = raw.SeriesID
		kind = "Primary"
	}
	if imageID == "" && raw.ParentThumbImageTag != "" && ValidID(raw.ParentThumbItemID) {
		imageID = raw.ParentThumbItemID
		kind = "Thumb"
	}
	if imageID != "" {
		item.Image = "/admin/api/v1/media/items/" + url.PathEscape(imageID) + "/images/" + kind + "?" + url.Values{"source_ref": {source}, "size": {"small"}}.Encode()
	}
	if detail {
		item.Overview = clean(raw.Overview, 8000)
		item.OfficialRating = clean(raw.OfficialRating, 40)
		item.Container = clean(raw.Container, 40)
		item.CommunityRating = raw.CommunityRating
		if item.CommunityRating != nil && (*item.CommunityRating < 0 || *item.CommunityRating > 10) {
			item.CommunityRating = nil
		}
		for _, genre := range raw.Genres {
			if len(item.Genres) == 20 {
				break
			}
			if genre = clean(genre, 80); genre != "" {
				item.Genres = append(item.Genres, genre)
			}
		}
		for _, track := range raw.MediaStreams {
			if len(item.MediaStreams) == 40 {
				break
			}
			if track.Type != "Video" && track.Type != "Audio" && track.Type != "Subtitle" {
				continue
			}
			item.MediaStreams = append(item.MediaStreams, Stream{Type: track.Type, Codec: clean(track.Codec, 40), Language: clean(track.Language, 40), Title: clean(track.DisplayTitle, 200), Width: positive(track.Width), Height: positive(track.Height), Channels: positive(track.Channels), IsDefault: track.IsDefault, IsExternal: track.IsExternal})
		}
	}
	return item
}

func clean(value string, max int) string {
	value = strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' || r == 127 {
			return -1
		}
		return r
	}, value)
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > max {
		runes = runes[:max]
	}
	return string(runes)
}
func nonnegative(v *int) *int {
	if v != nil && *v < 0 {
		return nil
	}
	return v
}
func positive(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
