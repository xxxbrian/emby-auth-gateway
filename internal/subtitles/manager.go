package subtitles

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/sourcecache"
	"github.com/xxxbrian/emby-auth-gateway/internal/subtitleindex"
)

const (
	maxJobs         = 64
	maxTracks       = 64
	maxGrants       = 2048
	leaseLifetime   = 2 * time.Minute
	previewLifetime = time.Minute
	probeLifetime   = 5 * time.Minute
	retryDelay      = 30 * time.Second
)

type trackState struct {
	track                            Track
	state, reason, artifact, version string
	mode                             string
	indexed, strong                  bool
	format                           string
	cues, requests                   int
	readBytes                        int64
	checked                          time.Time
}

type viewer struct {
	input    Input
	touched  time.Time
	selected *int
	playback bool
	recover  bool
}

type job struct {
	key, id, itemID, sourceID, name string
	size                            int64
	created, updated                time.Time
	tracks                          map[int]*trackState
	viewers                         map[string]*viewer
	cancel                          context.CancelFunc
	running                         bool
	changed                         chan struct{}
}

type grant struct {
	id, owner, playback, item, jobKey string
	index                             int
	artifact, version                 string
	indexed                           bool
	strong                            bool
	input                             Input
	last                              time.Time
}

type Manager struct {
	mu        sync.Mutex
	cfg       Config
	ctx       context.Context
	cancel    context.CancelFunc
	closed    bool
	closeOnce sync.Once
	closeErr  error
	wg        sync.WaitGroup
	jobs      map[string]*job
	grants    map[string]*grant
	store     *store
	source    *sourcecache.Cache
	active    int
}

func New(cfg Config) (*Manager, error) {
	if cfg.CacheBytes == 0 {
		cfg.CacheBytes = 128 << 20
	}
	if cfg.SourceCacheBytes == 0 {
		cfg.SourceCacheBytes = 256 << 20
	}
	if cfg.Workers == 0 {
		cfg.Workers = 1
	}
	if cfg.PrepareTimeout == 0 {
		cfg.PrepareTimeout = 2 * time.Second
	}
	if cfg.WorkTimeout == 0 {
		cfg.WorkTimeout = 30 * time.Second
	}
	if cfg.MaxReadBytes == 0 {
		cfg.MaxReadBytes = 64 << 20
	}
	if cfg.MaxRequests == 0 {
		cfg.MaxRequests = 2048
	}
	if cfg.CacheBytes < 1 || cfg.SourceCacheBytes < 1 || cfg.Workers < 1 || cfg.Workers > 4 || cfg.PrepareTimeout < 0 || cfg.PrepareTimeout > 5*time.Second || cfg.WorkTimeout < 0 || cfg.WorkTimeout > 2*time.Minute || cfg.MaxReadBytes < 1 || cfg.MaxReadBytes > 128<<20 || cfg.MaxRequests < 1 || cfg.MaxRequests > 8192 {
		return nil, fmt.Errorf("invalid subtitle resource limits")
	}
	if cfg.BootID == "" {
		var err error
		cfg.BootID, err = randomID()
		if err != nil {
			return nil, err
		}
	}
	s, err := openStore(cfg.Dir, cfg.CacheBytes)
	if err != nil {
		return nil, err
	}
	ranges, err := sourcecache.New(sourcecache.Config{Dir: cfg.Dir, BudgetBytes: cfg.SourceCacheBytes, MaxEntries: 4096})
	if err != nil {
		_ = s.close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{cfg: cfg, ctx: ctx, cancel: cancel, jobs: map[string]*job{}, grants: map[string]*grant{}, store: s, source: ranges}
	m.wg.Add(1)
	go m.maintain()
	return m, nil
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func viewerKey(owner, playback string) string { return hash(owner, playback) }

func validInput(ctx context.Context, in Input) bool {
	return ctx.Err() == nil && in.Owner != "" && in.PlaybackID != "" && in.ItemID != "" && in.SourceID != "" && in.SourceKey != "" && len(in.SourceKey) <= 2048 && len(in.Tracks) <= maxTracks && in.Valid != nil && in.Valid(ctx)
}

// Prepare projects only artifacts already ready at return. Bounded preparation
// may continue for actual Web subscribers; catalog calls with Probe=false do
// no upstream work. An unavailable subtitle never fails a playback response.
func (m *Manager) Prepare(ctx context.Context, in Input, selection Selection) []Ready {
	if m == nil || !validInput(ctx, in) {
		return nil
	}
	in.Tracks = append([]Track(nil), in.Tracks...)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	key := hash(in.SourceKey, in.SourceID)
	j := m.jobs[key]
	if j == nil {
		if !selection.Probe || len(m.jobs) >= maxJobs {
			m.mu.Unlock()
			return nil
		}
		id, err := randomID()
		if err != nil {
			m.mu.Unlock()
			return nil
		}
		now := time.Now()
		j = &job{key: key, id: id, itemID: safeLabel(in.ItemID), sourceID: safeLabel(in.SourceID), name: safeLabel(in.SourceName), size: in.Size, created: now, updated: now, tracks: map[int]*trackState{}, viewers: map[string]*viewer{}, changed: make(chan struct{})}
		m.jobs[key] = j
	}
	now := time.Now()
	for _, t := range in.Tracks {
		if t.Index >= 0 && t.Index <= 1024 && j.tracks[t.Index] == nil && len(j.tracks) < maxTracks {
			j.tracks[t.Index] = &trackState{track: t, state: "unknown"}
		}
	}
	vk := viewerKey(in.Owner, in.PlaybackID)
	if len(j.viewers) >= maxGrants && j.viewers[vk] == nil {
		m.mu.Unlock()
		return nil
	}
	var selected *int
	if selection.Index != nil {
		index := *selection.Index
		selected = &index
	}
	existing := j.viewers[vk]
	created := existing == nil
	admission := &viewer{input: in, touched: now, selected: selected, playback: selection.Playback, recover: selection.Recover}
	if existing != nil && existing.playback && !selection.Playback {
		// Browsing the same item must not demote an active Web playback lease.
		admission.playback, admission.selected, admission.recover = true, existing.selected, existing.recover
	}
	// Each call gets a distinct admission pointer, so cancellation of an older
	// request cannot detach a newer request using this same viewer/source.
	j.viewers[vk] = admission
	if selection.Playback {
		m.adoptPreviewsLocked(j, vk, in, existing != nil && !existing.playback)
	}
	if selection.Probe && !j.running {
		m.startLocked(j)
	}
	wait := j.changed
	shouldWait := selection.Probe && j.running
	m.mu.Unlock()
	if shouldWait {
		timer := time.NewTimer(m.cfg.PrepareTimeout)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				m.cancelPrepare(ctx.Err(), j, vk, admission, created)
				return nil
			case <-timer.C:
				goto ready
			case <-wait:
			}
			m.mu.Lock()
			// A selected ready result can return immediately. Otherwise wait
			// until the bounded job or prepare deadline completes.
			done := !j.running
			if selected != nil {
				if t := j.tracks[*selected]; t != nil && t.state == "ready" {
					done = true
				}
			}
			wait = j.changed
			m.mu.Unlock()
			if done {
				break
			}
		}
	}
ready:
	if !validInput(ctx, in) {
		m.cancelPrepare(ctx.Err(), j, vk, admission, created)
		return nil
	}
	allowed := make(map[int]bool, len(in.Tracks))
	for _, track := range in.Tracks {
		allowed[track.Index] = in.ValidTrack == nil || in.ValidTrack(ctx, track)
	}
	if err := ctx.Err(); err != nil {
		m.cancelPrepare(err, j, vk, admission, created)
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || j.viewers[vk] == nil {
		return nil
	}
	return m.readyLocked(j, in, allowed)
}

// Once actual playback owns this source, its earlier detail/negotiation
// previews are no longer independent reasons to keep optional work alive.
// Existing preview URLs follow the actual playback, while other sources,
// items, and viewers keep their own leases.
func (m *Manager) adoptPreviewsLocked(j *job, actualKey string, in Input, retired bool) {
	for key, v := range j.viewers {
		if key == actualKey || v.playback || v.input.Owner != in.Owner || v.input.ItemID != in.ItemID {
			continue
		}
		delete(j.viewers, key)
		retired = true
		for _, g := range m.grants {
			if g.jobKey == j.key && g.owner == in.Owner && g.item == in.ItemID && g.playback == v.input.PlaybackID {
				g.playback, g.input, g.last = in.PlaybackID, in, time.Now()
			}
		}
	}
	if !retired || j.cancel == nil {
		return
	}
	// Explicit Off must not inherit preview-only extraction. A different
	// viewer's requested recovery remains a valid reason to finish shared work.
	for _, v := range j.viewers {
		if (v.playback || v.recover) && v.selected != nil && *v.selected >= 0 {
			return
		}
	}
	j.cancel()
}

// A shared PlaybackInfo deadline can expire while preparing one source after
// another source's grants have already been advertised. It is a preparation
// budget, not a playback stop. Only explicit End releases playback-wide leases.
// A disconnected request may detach its own still-unclaimed, newly-created
// source admission; existing admissions and newer requests remain untouched.
func (m *Manager) cancelPrepare(err error, j *job, key string, admission *viewer, created bool) {
	if !errors.Is(err, context.Canceled) || !created {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if j.viewers[key] != admission {
		return
	}
	for _, g := range m.grants {
		if g.jobKey == j.key && g.owner == admission.input.Owner && g.playback == admission.input.PlaybackID {
			return
		}
	}
	delete(j.viewers, key)
	if len(j.viewers) == 0 && j.cancel != nil {
		j.cancel()
	}
}

func (m *Manager) readyLocked(j *job, in Input, allowed map[int]bool) []Ready {
	var ready []Ready
	for _, track := range in.Tracks {
		if !allowed[track.Index] {
			continue
		}
		t := j.tracks[track.Index]
		if t == nil || t.state != "ready" {
			continue
		}
		a := m.store.files[t.artifact]
		if a == nil {
			t.state = "unknown"
			continue
		}
		var g *grant
		for _, existing := range m.grants {
			if existing.owner == in.Owner && existing.playback == in.PlaybackID && existing.item == in.ItemID && existing.jobKey == j.key && existing.index == track.Index && existing.artifact == t.artifact {
				g = existing
				break
			}
		}
		if g == nil {
			if len(m.grants) >= maxGrants {
				continue
			}
			id, err := randomID()
			if err != nil {
				continue
			}
			g = &grant{id: id, owner: in.Owner, playback: in.PlaybackID, item: in.ItemID, jobKey: j.key, index: track.Index, artifact: t.artifact, version: t.version, indexed: t.indexed, strong: t.strong, input: in, last: time.Now()}
			m.grants[id] = g
			a.pins++
		} else {
			g.input = in
			g.last = time.Now()
		}
		ready = append(ready, Ready{Index: track.Index, ID: g.id, Format: a.format})
	}
	return ready
}

func (m *Manager) signalLocked(j *job) {
	close(j.changed)
	j.changed = make(chan struct{})
	j.updated = time.Now()
}

func (m *Manager) startLocked(j *job) {
	if m.active >= m.cfg.Workers {
		return
	}
	// Do not re-probe known failures on every Web poll.
	wanted := false
	for _, t := range j.tracks {
		_, selected := m.inputLocked(j, t.track.Index, true)
		if selected && t.reason == "upstream_empty" && !t.track.External && (strings.EqualFold(t.track.Codec, "subrip") || strings.EqualFold(t.track.Codec, "srt")) {
			wanted = true
			break
		}
		if t.state == "unknown" || t.state != "ready" && time.Since(t.checked) >= retryDelay || t.state == "ready" && !t.indexed && time.Since(t.checked) >= probeLifetime {
			wanted = true
			break
		}
	}
	if !wanted {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, m.cfg.WorkTimeout)
	j.cancel = cancel
	j.running = true
	m.active++
	m.wg.Add(1)
	go m.run(ctx, j)
}

func (m *Manager) run(ctx context.Context, j *job) {
	defer m.wg.Done()
	defer func() {
		m.mu.Lock()
		j.running = false
		j.cancel()
		j.cancel = nil
		m.active--
		m.signalLocked(j)
		m.mu.Unlock()
	}()
	// Probe upstream documents before any expensive indexed recovery, so a
	// good external ASS track is not held behind a slow MKV extraction.
	m.mu.Lock()
	indices := make([]int, 0, len(j.tracks))
	for index := range j.tracks {
		indices = append(indices, index)
	}
	priority := func(index int) int {
		if _, selected := m.inputLocked(j, index, true); selected {
			return 0
		}
		t := j.tracks[index].track
		if t.External {
			return 1
		}
		if strings.EqualFold(t.Codec, "ass") || strings.EqualFold(t.Codec, "ssa") {
			return 2
		}
		return 3
	}
	sort.Slice(indices, func(i, k int) bool {
		a, b := priority(indices[i]), priority(indices[k])
		if a != b {
			return a < b
		}
		return indices[i] < indices[k]
	})
	m.mu.Unlock()
	probes := 0
	for _, index := range indices {
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		t := j.tracks[index]
		in, ok := m.inputLocked(j, index, false)
		probe := t.state == "unknown" || t.state != "ready" && time.Since(t.checked) >= retryDelay || t.state == "ready" && !t.indexed && time.Since(t.checked) >= probeLifetime
		if probe {
			t.state = "probing"
			t.mode = "upstream"
			t.reason = ""
			m.signalLocked(j)
		}
		m.mu.Unlock()
		if !ok {
			return
		}
		if !probe {
			continue
		}
		if probes >= 4 {
			m.mu.Lock()
			t.state = "unknown"
			m.mu.Unlock()
			continue
		}
		if !textCodec(t.track.Codec) || in.Fetch == nil {
			m.failed(j, t, "unsupported")
			continue
		}
		fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		probes++
		var data []byte
		var format string
		var err error
		if in.Valid(fetchCtx) {
			data, format, err = in.Fetch(fetchCtx, t.track)
		} else {
			err = ErrUnavailable
		}
		cancel()
		if err != nil {
			m.failed(j, t, "upstream_unavailable")
			continue
		}
		if len(data) == 0 {
			m.failed(j, t, "upstream_empty")
			continue
		}
		data, format, cues, err := normalizeText(data, format)
		if err != nil {
			reason := "invalid_subtitle"
			if errors.Is(err, errEmptyDocument) {
				reason = "upstream_empty"
			}
			m.failed(j, t, reason)
			continue
		}
		key := hash("upstream-v1", m.cfg.BootID, in.SourceKey, strconv.Itoa(index), t.track.Codec, hash(string(data)))
		m.publish(ctx, j, t, key, data, format, cues, "", false, false, 0, 0)
	}
	for _, index := range indices {
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		t := j.tracks[index]
		in, ok := m.inputLocked(j, index, true)
		eligible := ok && t.reason == "upstream_empty" && !t.track.External && (strings.EqualFold(t.track.Codec, "subrip") || strings.EqualFold(t.track.Codec, "srt"))
		if eligible {
			t.state = "extracting"
			t.mode = "indexed"
			t.reason = ""
			m.signalLocked(j)
		}
		m.mu.Unlock()
		if !eligible {
			continue
		}
		m.mu.Lock()
		playback := false
		for _, v := range j.viewers {
			if v.playback && v.selected != nil && *v.selected == index {
				playback = true
				break
			}
		}
		m.mu.Unlock()
		m.extract(ctx, j, t, in, playback)
	}
}

func (m *Manager) inputLocked(j *job, index int, selectedOnly bool) (Input, bool) {
	for _, v := range j.viewers {
		if !selectedOnly || (v.playback || v.recover) && v.selected != nil && *v.selected == index {
			return v.input, true
		}
	}
	return Input{}, false
}

func (m *Manager) failed(j *job, t *trackState, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t.state = "unavailable"
	t.reason = reason
	t.checked = time.Now()
	m.signalLocked(j)
}

func (m *Manager) extract(ctx context.Context, j *job, t *trackState, in Input, playback bool) {
	readLimit, requestLimit := m.cfg.MaxReadBytes, m.cfg.MaxRequests
	if !playback {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		readLimit = min(readLimit, 8<<20)
		requestLimit = min(requestLimit, 128)
	}
	if in.Validate == nil || in.Open == nil || !in.Valid(ctx) {
		m.failed(j, t, "source_unavailable")
		return
	}
	version, strong, err := in.Validate(ctx)
	if err != nil {
		m.failed(j, t, "source_unavailable")
		return
	}
	if !strong || version == "" {
		// Indexed packets may span many range responses. Without a strong
		// validator we cannot prove they belong to one immutable source, even
		// within this process. Prefer no caption to a speculative mixed result.
		m.failed(j, t, "source_validator_missing")
		return
	}
	key := hash("indexed-v1", in.SourceKey, version, strconv.Itoa(t.track.Index), t.track.Codec, t.track.Language)
	m.mu.Lock()
	a := m.store.files[key]
	var cached []byte
	if a != nil {
		if f, e := m.store.read(a); e == nil {
			cached, _ = io.ReadAll(io.LimitReader(f, maxOutput+1))
			_ = f.Close()
		}
	}
	m.mu.Unlock()
	if len(cached) > 0 {
		if data, format, cues, e := normalizeText(cached, "vtt"); e == nil {
			m.publish(ctx, j, t, key, data, format, cues, version, true, strong, 0, 0)
			return
		}
	}
	if a != nil {
		m.mu.Lock()
		removed := m.store.discard(a)
		m.mu.Unlock()
		if !removed {
			m.failed(j, t, "cache_capacity_or_storage")
			return
		}
	}
	open := func(readCtx context.Context, offset, length int64) (io.ReadCloser, error) {
		if !in.Valid(readCtx) {
			return nil, ErrUnavailable
		}
		return m.ReadSource(readCtx, SourceGenerationKey(in.SourceKey, version), offset, length, in.Open)
	}
	result, err := subtitleindex.Extract(ctx, subtitleindex.Source{Size: in.Size, Container: in.Container, Open: open}, subtitleindex.Track{Index: t.track.Index, Codec: t.track.Codec, Language: t.track.Language, Name: t.track.Name}, subtitleindex.Limits{MaxReadBytes: readLimit, MaxRequests: requestLimit, MaxOutputBytes: maxOutput, MaxCues: 20000})
	if err != nil {
		reason := "source_unavailable"
		switch {
		case errors.Is(err, subtitleindex.ErrLimit):
			reason = "resource_limit"
		case errors.Is(err, subtitleindex.ErrIndex):
			reason = "missing_index"
		case errors.Is(err, subtitleindex.ErrUnsupported):
			reason = "unsupported"
		case errors.Is(err, subtitleindex.ErrEmpty):
			reason = "empty_indexed_packets"
		case ctx.Err() != nil:
			reason = "canceled_or_timeout"
		}
		m.mu.Lock()
		t.readBytes = result.ReadBytes
		t.requests = result.Requests
		m.mu.Unlock()
		m.failed(j, t, reason)
		return
	}
	// Validate once more before publishing: partial content from different
	// versions must never become a reusable subtitle artifact.
	endVersion, endStrong, err := in.Validate(ctx)
	if err != nil || endVersion != version || strong && !endStrong || !in.Valid(ctx) {
		m.failed(j, t, "source_changed")
		return
	}
	data, format, cues, err := normalizeText(result.Data, result.Format)
	if err != nil {
		m.failed(j, t, "invalid_subtitle")
		return
	}
	m.publish(ctx, j, t, key, data, format, cues, version, true, strong, result.ReadBytes, result.Requests)
}

func (m *Manager) publish(ctx context.Context, j *job, t *trackState, key string, data []byte, format string, cues int, version string, indexed, strong bool, readBytes int64, requests int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || ctx.Err() != nil || len(j.viewers) == 0 {
		t.state = "unavailable"
		t.reason = "canceled_or_timeout"
		t.checked = time.Now()
		m.signalLocked(j)
		return
	}
	a, err := m.store.put(key, format, data)
	if err != nil {
		t.state = "unavailable"
		t.reason = "cache_capacity_or_storage"
	} else {
		t.state = "ready"
		t.reason = ""
		t.artifact = a.key
		t.format = format
		t.cues = cues
		t.version = version
		t.indexed = indexed
		t.strong = strong
	}
	t.readBytes = readBytes
	t.requests = requests
	t.checked = time.Now()
	m.signalLocked(j)
}

// Open authenticates the scoped grant again, including current source identity
// for extracted captions. It returns a pinned seekable file; caller must Close.
func (m *Manager) Open(ctx context.Context, owner, itemID, grantID string) (io.ReadSeekCloser, string, error) {
	if m == nil {
		return nil, "", ErrUnavailable
	}
	m.mu.Lock()
	g := m.grants[grantID]
	if m.closed || g == nil || g.owner != owner || g.item != itemID || time.Since(g.last) > leaseLifetime {
		m.mu.Unlock()
		return nil, "", ErrUnavailable
	}
	copy := *g
	m.mu.Unlock()
	if !validInput(ctx, copy.input) {
		return nil, "", ErrUnavailable
	}
	if copy.input.ValidTrack != nil {
		allowed := false
		for _, track := range copy.input.Tracks {
			if track.Index == copy.index {
				allowed = copy.input.ValidTrack(ctx, track)
				break
			}
		}
		if !allowed {
			return nil, "", ErrUnavailable
		}
	}
	if copy.indexed {
		if copy.input.Validate == nil {
			return nil, "", ErrUnavailable
		}
		version, strong, err := copy.input.Validate(ctx)
		if err != nil || version != copy.version || copy.strong && !strong {
			if err == nil {
				m.mu.Lock()
				if j := m.jobs[copy.jobKey]; j != nil {
					if t := j.tracks[copy.index]; t != nil && t.artifact == copy.artifact {
						t.state = "unavailable"
						t.reason = "source_changed"
						t.checked = time.Now()
						m.signalLocked(j)
					}
				}
				m.mu.Unlock()
			}
			return nil, "", ErrUnavailable
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.grants[grantID] != g {
		return nil, "", ErrUnavailable
	}
	a := m.store.files[g.artifact]
	if a == nil {
		return nil, "", ErrUnavailable
	}
	f, err := m.store.read(a)
	if err != nil {
		return nil, "", ErrUnavailable
	}
	a.pins++
	g.last = time.Now()
	return &artifactReader{ReadSeekCloser: f, m: m, a: a}, a.format, nil
}

type artifactReader struct {
	io.ReadSeekCloser
	m    *Manager
	a    *artifact
	once sync.Once
	err  error
}

func (r *artifactReader) Close() error {
	r.once.Do(func() { r.err = r.ReadSeekCloser.Close(); r.m.mu.Lock(); r.a.pins--; r.m.mu.Unlock() })
	return r.err
}

func (m *Manager) End(owner, playbackID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.endLocked(owner, playbackID)
}
func (m *Manager) endLocked(owner, playbackID string) {
	for _, j := range m.jobs {
		delete(j.viewers, viewerKey(owner, playbackID))
		if len(j.viewers) == 0 && j.cancel != nil {
			j.cancel()
		}
	}
	for id, g := range m.grants {
		if g.owner == owner && g.playback == playbackID {
			if a := m.store.files[g.artifact]; a != nil {
				a.pins--
			}
			delete(m.grants, id)
		}
	}
}

func (m *Manager) Touch(owner, playbackID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for _, j := range m.jobs {
		if v := j.viewers[viewerKey(owner, playbackID)]; v != nil {
			v.touched = now
		}
	}
	for _, g := range m.grants {
		if g.owner == owner && g.playback == playbackID {
			g.last = now
		}
	}
}

func (m *Manager) MovePlayback(owner, oldID, newID string) {
	if m == nil || oldID == newID || newID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		oldKey := viewerKey(owner, oldID)
		if v := j.viewers[oldKey]; v != nil {
			delete(j.viewers, oldKey)
			newKey := viewerKey(owner, newID)
			if current := j.viewers[newKey]; current != nil {
				// The destination is the newly negotiated playback. Its input
				// and subtitle choice supersede the old playback's selection.
				// Merge the lease without restoring an old language or Off state.
				current.playback = current.playback || v.playback
				if v.touched.After(current.touched) {
					current.touched = v.touched
				}
			} else {
				v.input.PlaybackID = newID
				j.viewers[newKey] = v
			}
		}
	}
	for _, g := range m.grants {
		if g.owner == owner && g.playback == oldID {
			g.playback = newID
			g.input.PlaybackID = newID
			if j := m.jobs[g.jobKey]; j != nil {
				if current := j.viewers[viewerKey(owner, newID)]; current != nil && current.input.ItemID == g.item {
					g.input = current.input
				}
			}
		}
	}
}

func (m *Manager) maintain() {
	defer m.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-ticker.C:
			m.mu.Lock()
			for _, j := range m.jobs {
				for _, v := range j.viewers {
					lifetime := previewLifetime
					if v.playback {
						lifetime = leaseLifetime
					}
					if now.Sub(v.touched) > lifetime {
						m.endLocked(v.input.Owner, v.input.PlaybackID)
					}
				}
			}
			for key, j := range m.jobs {
				if !j.running && len(j.viewers) == 0 && now.Sub(j.updated) > probeLifetime {
					delete(m.jobs, key)
				}
			}
			m.mu.Unlock()
		}
	}
}

func (m *Manager) HasActiveWork() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active > 0
}

// SourceGenerationKey must also be used by Web audio range capture to share
// bytes with extraction without conflating mutable source generations.
func SourceGenerationKey(base, version string) string {
	return hash("source-generation-v1", base, version)
}

func (m *Manager) ReadSource(ctx context.Context, key string, offset, length int64, fetch func(context.Context, int64, int64) (io.ReadCloser, error)) (io.ReadCloser, error) {
	if m == nil {
		return fetch(ctx, offset, length)
	}
	return m.source.Open(ctx, hash(key), offset, length, fetch)
}

func (m *Manager) CaptureSource(key string, offset int64, body io.ReadCloser) io.ReadCloser {
	if m == nil {
		return body
	}
	return m.source.Capture(hash(key), offset, body)
}

func (m *Manager) Snapshot() Snapshot {
	if m == nil {
		return Snapshot{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{Enabled: !m.closed, BootID: m.cfg.BootID, Workers: m.cfg.Workers, Active: m.active, CacheBytes: m.store.used, CacheBudget: m.cfg.CacheBytes, SourceCacheBudget: m.cfg.SourceCacheBytes, Grants: len(m.grants), Jobs: []JobView{}}
	for _, j := range m.jobs {
		v := JobView{ID: j.id, ItemID: j.itemID, SourceID: j.sourceID, SourceName: j.name, SourceSize: j.size, State: "idle", CreatedAt: j.created, UpdatedAt: j.updated, Viewers: len(j.viewers), Tracks: []TrackView{}}
		if j.running {
			v.State = "preparing"
		}
		for _, t := range j.tracks {
			size := int64(0)
			if a := m.store.files[t.artifact]; a != nil {
				size = a.size
			}
			mode := t.mode
			if mode == "" {
				mode = "upstream"
			}
			v.Tracks = append(v.Tracks, TrackView{Index: t.track.Index, Language: safeLabel(t.track.Language), Name: safeLabel(t.track.Name), State: t.state, Format: t.format, Mode: mode, Persistent: t.indexed && t.strong, Bytes: size, Cues: t.cues, ReadBytes: t.readBytes, Requests: t.requests, Reason: t.reason})
		}
		sort.Slice(v.Tracks, func(i, k int) bool { return v.Tracks[i].Index < v.Tracks[k].Index })
		s.Jobs = append(s.Jobs, v)
	}
	sort.Slice(s.Jobs, func(i, j int) bool { return s.Jobs[i].CreatedAt.After(s.Jobs[j].CreatedAt) })
	ranges := m.source.Snapshot()
	s.SourceCacheBytes = ranges.CachedBytes
	s.SourceReadBytes = int64(ranges.SourceReadBytes)
	s.SourceHitBytes = int64(ranges.CacheHitBytes)
	return s
}

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.cancel()
		m.mu.Unlock()
		m.wg.Wait()
		m.closeErr = m.source.Close()
		m.mu.Lock()
		m.closeErr = errors.Join(m.closeErr, m.store.close())
		m.mu.Unlock()
	})
	return m.closeErr
}
