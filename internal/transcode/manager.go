package transcode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const lookaheadSegments = 4

type Manager struct {
	cfg         Config
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	jobs        map[string]*job
	inputs      map[string]*job
	queue       chan *job
	probes      chan struct{}
	cache       *catalog
	lock        *os.File
	server      *http.Server
	inputBase   string
	version     string
	decoders    map[string]bool
	closed      bool
	wg          sync.WaitGroup
	observation atomic.Pointer[Observation]
	recent      []JobView
}
type job struct {
	playbackID, replacement string
	id, inputKey            string
	identity                Identity
	source                  Source
	plan                    Plan
	initial                 int64
	ctx                     context.Context
	cancel                  context.CancelFunc
	timeline                Timeline
	prepared                chan struct{}
	prepareErr              error
	preparing               bool
	changed                 chan struct{}
	demands                 map[int]*demand
	nextDemand              uint64
	queued, running, closed bool
	state, failure          string
	runCancel               context.CancelFunc
	generation              uint64
	runs                    int
	pid                     int
	init                    []byte
	tracks                  []movieTrack
	created, lastUse        time.Time
	position                *int64
	paused                  *bool
	positionAt              *time.Time
	requested, produced     int64
	runStarted              time.Time
	runStartTicks           int64
}
type demand struct {
	count int
	order uint64
}

func New(ctx context.Context, cfg Config) (*Manager, error) {
	if cfg.Workers == 0 {
		cfg.Workers = 4
	}
	if cfg.Workers < 1 || cfg.Workers > 32 {
		return nil, fmt.Errorf("audio worker limit must be between 1 and 32")
	}
	if cfg.CacheBytes == 0 {
		cfg.CacheBytes = 4 << 30
	}
	if cfg.CacheBytes < 16<<20 {
		return nil, fmt.Errorf("audio cache budget must be at least 16 MiB")
	}
	if cfg.MaxJobs == 0 {
		cfg.MaxJobs = 128
	}
	if cfg.MaxJobs < cfg.Workers || cfg.MaxJobs > 1024 {
		return nil, fmt.Errorf("invalid audio playback limit")
	}
	if cfg.IdleTTL == 0 {
		cfg.IdleTTL = 6 * time.Hour
	}
	if cfg.WorkTimeout == 0 {
		cfg.WorkTimeout = 90 * time.Second
	}
	if cfg.FFmpegPath == "" {
		cfg.FFmpegPath = "ffmpeg"
	}
	path, err := exec.LookPath(cfg.FFmpegPath)
	if err != nil {
		return nil, fmt.Errorf("audio conversion requires ffmpeg: %w", err)
	}
	cfg.FFmpegPath = path
	version, decoders, err := checkFFmpeg(ctx, path)
	if err != nil {
		return nil, err
	}
	if cfg.CacheDir == "" {
		return nil, fmt.Errorf("audio cache directory is required")
	}
	root, lock, err := openCache(cfg.CacheDir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	m := &Manager{cfg: cfg, ctx: ctx, cancel: cancel, jobs: map[string]*job{}, inputs: map[string]*job{}, queue: make(chan *job, cfg.MaxJobs), probes: make(chan struct{}, cfg.Workers), cache: &catalog{root: root, budget: cfg.CacheBytes, entries: map[string]*artifact{}}, lock: lock, version: version, decoders: decoders}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		_ = lock.Close()
		return nil, err
	}
	m.inputBase = "http://" + listener.Addr().String() + "/input/"
	m.server = &http.Server{Handler: http.HandlerFunc(m.serveInput), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	m.wg.Add(1)
	go func() { defer m.wg.Done(); _ = m.server.Serve(listener) }()
	for i := 0; i < cfg.Workers; i++ {
		m.wg.Add(1)
		go m.worker()
	}
	m.publishObservation()
	m.wg.Add(1)
	go m.observe()
	m.wg.Add(1)
	go m.maintain()
	return m, nil
}

func token() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable")
	}
	return hex.EncodeToString(b[:])
}

func checkFFmpeg(ctx context.Context, path string) (string, map[string]bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var version string
	decoders := map[string]bool{}
	for _, flag := range []string{"-version", "-encoders", "-decoders", "-muxers", "-bsfs"} {
		out, err := exec.CommandContext(ctx, path, "-hide_banner", flag).Output()
		if err != nil || len(out) > 2<<20 {
			return "", nil, fmt.Errorf("unable to verify ffmpeg %s", flag)
		}
		s := string(out)
		switch flag {
		case "-version":
			version = strings.SplitN(s, "\n", 2)[0]
		case "-encoders":
			if !strings.Contains(s, " aac ") {
				return "", nil, fmt.Errorf("ffmpeg AAC encoder is unavailable")
			}
		case "-muxers":
			if !strings.Contains(s, " mp4 ") {
				return "", nil, fmt.Errorf("ffmpeg MP4 muxer is unavailable")
			}
		case "-bsfs":
			if !strings.Contains(s, "hevc_mp4toannexb") || !strings.Contains(s, "extract_extradata") {
				return "", nil, fmt.Errorf("ffmpeg video packet filters are unavailable")
			}
		case "-decoders":
			for _, line := range strings.Split(s, "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					decoders[fields[1]] = true
				}
			}
		}
	}
	decoders["dts"] = decoders["dca"]
	return version, decoders, nil
}

func openCache(dir string) (string, *os.File, error) {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return "", nil, err
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", nil, fmt.Errorf("audio cache must be a directory, not a symlink")
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", nil, err
	}
	fail := func(err error) (string, *os.File, error) { _ = lock.Close(); return "", nil, err }
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(fmt.Errorf("audio cache is already in use"))
	}
	marker := filepath.Join(dir, ".gateway-audio-cache")
	b, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		entries, e := os.ReadDir(dir)
		if e != nil {
			return fail(e)
		}
		for _, entry := range entries {
			if entry.Name() != ".lock" {
				return fail(fmt.Errorf("audio cache directory is not empty or gateway-owned"))
			}
		}
		if err = os.WriteFile(marker, []byte("gateway-audio-cache-v1\n"), 0600); err != nil {
			return fail(err)
		}
	} else if err != nil || string(b) != "gateway-audio-cache-v1\n" {
		return fail(fmt.Errorf("audio cache ownership marker is invalid"))
	}
	root := filepath.Join(dir, "segments")
	if err = os.RemoveAll(root); err != nil {
		return fail(err)
	}
	if err = os.Mkdir(root, 0700); err != nil {
		return fail(err)
	}
	return root, lock, nil
}

// Create registers an immutable plan. Source metadata and media are loaded only
// when the player requests its manifest or segments.
func (m *Manager) Create(identity Identity, source Source, plan Plan, start int64) (string, error) {
	if source.Open == nil || identity.Owner == "" || plan.AudioCodec != "copy" && !m.decoders[plan.Audio.Codec] {
		return "", ErrUnsupported
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", ErrNotFound
	}
	if len(m.jobs) >= m.cfg.MaxJobs {
		return "", ErrCapacity
	}
	id := "eag-" + token()
	ctx, cancel := context.WithCancel(m.ctx)
	now := time.Now()
	j := &job{id: id, inputKey: token(), identity: identity, source: source, plan: plan, initial: start, ctx: ctx, cancel: cancel, changed: make(chan struct{}), demands: map[int]*demand{}, state: "ready", created: now, lastUse: now}
	m.jobs[id] = j
	j.playbackID = id
	m.inputs[j.inputKey] = j
	return id, nil
}

func (m *Manager) get(owner, id string) (*job, error) {
	j := m.jobs[id]
	if j == nil || j.closed || j.identity.Owner != owner {
		return nil, ErrNotFound
	}
	return j, nil
}

func (m *Manager) OwnsItem(owner, id, itemID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.get(owner, id)
	return err == nil && j.identity.ItemID == itemID
}

func (m *Manager) Container(owner, id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.get(owner, id)
	if err != nil {
		return ""
	}
	return j.plan.Container
}
func changed(j *job) { close(j.changed); j.changed = make(chan struct{}) }

func (m *Manager) prepare(ctx context.Context, owner, id string) (*job, error) {
	m.mu.Lock()
	j, err := m.get(owner, id)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	j.lastUse = time.Now()
	if j.prepared == nil {
		j.prepared = make(chan struct{})
		j.preparing = true
		j.state = "preparing"
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			ctx, cancel := context.WithTimeout(j.ctx, 30*time.Second)
			defer cancel()
			var timeline Timeline
			var err error
			select {
			case m.probes <- struct{}{}:
				timeline, err = ReadTimeline(ctx, j.source)
				<-m.probes
			case <-ctx.Done():
				err = ctx.Err()
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			j.preparing = false
			j.timeline, j.prepareErr = timeline, err
			if !j.closed {
				if err != nil {
					j.state = "failed"
					j.failure = errorCode(err)
				} else {
					j.state = "ready"
				}
			}
			close(j.prepared)
			changed(j)
		}()
	}
	done := j.prepared
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-j.ctx.Done():
		return nil, ErrNotFound
	case <-done:
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if j.closed {
		return nil, ErrNotFound
	}
	return j, j.prepareErr
}

func (m *Manager) Manifest(ctx context.Context, owner, id string) (string, error) {
	j, err := m.prepare(ctx, owner, id)
	if err != nil {
		return "", err
	}
	t := j.timeline
	var b strings.Builder
	target := int64(1)
	for i := 0; i < t.Len(); i++ {
		target = max(target, (t.Cuts[i+1]-t.Cuts[i]+TicksPerSecond-1)/TicksPerSecond)
	}
	version, extension := 7, "m4s"
	if j.plan.Container == "ts" {
		version, extension = 3, "ts"
	}
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:%d\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n", version, target)
	if j.plan.Container == "mp4" {
		b.WriteString("#EXT-X-MAP:URI=\"init.mp4\"\n")
	}
	for i := 0; i < t.Len(); i++ {
		fmt.Fprintf(&b, "#EXTINF:%s,\n%d.%s\n", seconds(t.Cuts[i+1]-t.Cuts[i]), i, extension)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String(), nil
}

func (m *Manager) enqueue(j *job) {
	if j.queued || j.running || j.closed {
		return
	}
	j.queued = true
	j.state = "queued"
	select {
	case m.queue <- j:
	default:
		j.queued = false
		j.state, j.failure = "failed", "capacity_exhausted"
		changed(j)
	}
}

// Open waits for one immutable artifact and returns a pinned reader. Init data
// is small bounded metadata and survives media-cache eviction for this plan.
func (m *Manager) Open(ctx context.Context, owner, id string, index int) (io.ReadSeekCloser, int64, error) {
	j, err := m.prepare(ctx, owner, id)
	if err != nil {
		return nil, 0, err
	}
	if index < -1 || index >= j.timeline.Len() {
		return nil, 0, ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, m.cfg.WorkTimeout+10*time.Second)
	defer cancel()
	m.mu.Lock()
	j.nextDemand++
	d := j.demands[index]
	if d == nil {
		d = &demand{order: j.nextDemand}
		j.demands[index] = d
	}
	d.count++
	if index >= 0 {
		m.cache.protect(j.id+"/"+strconv.Itoa(index), 1)
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		d.count--
		if index >= 0 {
			m.cache.protect(j.id+"/"+strconv.Itoa(index), -1)
		}
		if d.count == 0 {
			delete(j.demands, index)
		}
	}()
	for {
		m.mu.Lock()
		if j.closed {
			m.mu.Unlock()
			return nil, 0, ErrNotFound
		}
		j.lastUse = time.Now()
		if index == -1 && j.init != nil {
			data := j.init
			m.mu.Unlock()
			return &memoryFile{Reader: strings.NewReader(string(data))}, int64(len(data)), nil
		}
		if index >= 0 {
			if f, e := m.cache.open(j.id + "/" + strconv.Itoa(index)); e == nil {
				m.mu.Unlock()
				return f, f.entry.size, nil
			}
		}
		if j.failure != "" {
			code := j.failure
			m.mu.Unlock()
			return nil, 0, codeError(code)
		}
		m.enqueue(j)
		if j.failure != "" {
			code := j.failure
			m.mu.Unlock()
			return nil, 0, codeError(code)
		}
		ch := j.changed
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-j.ctx.Done():
			return nil, 0, ErrNotFound
		case <-ch:
		}
	}
}

type memoryFile struct{ *strings.Reader }

func (*memoryFile) Close() error { return nil }

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case j := <-m.queue:
			m.work(j)
		}
	}
}
func (m *Manager) work(j *job) {
	m.mu.Lock()
	j.queued = false
	if j.closed || j.prepareErr != nil {
		m.mu.Unlock()
		return
	}
	start := -1
	order := ^uint64(0)
	for index, d := range j.demands {
		if d.count <= 0 || index == -1 && j.init != nil || index >= 0 && m.cache.has(j.id+"/"+strconv.Itoa(index)) {
			continue
		}
		if d.order < order {
			order = d.order
			start = index
			if start == -1 {
				start = j.timeline.Segment(j.initial)
			}
		}
	}
	if start < 0 {
		j.state = "ready"
		m.mu.Unlock()
		return
	}
	end := min(j.timeline.Len(), start+lookaheadSegments)
	ctx, cancel := context.WithTimeout(j.ctx, m.cfg.WorkTimeout)
	j.runCancel = cancel
	j.generation++
	generation := j.generation
	j.running = true
	j.state = "producing"
	j.runs++
	j.requested = j.timeline.Cuts[start]
	j.runStartTicks = j.timeline.Cuts[max(0, start-1)]
	j.produced = j.runStartTicks
	j.runStarted = time.Now()
	changed(j)
	m.mu.Unlock()
	valid := func() bool { return !j.closed && j.generation == generation }
	err := runFFmpeg(ctx, m.cfg.FFmpegPath, runSpec{Input: m.inputBase + j.inputKey, Plan: j.plan, Timeline: j.timeline, Start: start, End: end,
		Started: func(pid int) {
			m.mu.Lock()
			defer m.mu.Unlock()
			if valid() {
				j.pid = pid
			}
		},
		Init: func(data []byte, tracks []movieTrack) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			if !valid() {
				return context.Canceled
			}
			if j.init == nil {
				j.init = append([]byte(nil), data...)
				j.tracks = tracks
				changed(j)
			} else if !sameTracks(j.tracks, tracks) {
				return ErrSource
			}
			return nil
		},
		Writer: func(index int) (*cacheWriter, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			if !valid() {
				return nil, context.Canceled
			}
			return m.cache.begin(j.id + "/" + strconv.Itoa(index))
		},
		Commit: func(index int, w *cacheWriter) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			if !valid() {
				return context.Canceled
			}
			if err := w.commit(); err != nil {
				return err
			}
			j.produced = j.timeline.Cuts[index+1]
			changed(j)
			return nil
		},
	})
	cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	j.running = false
	j.runCancel = nil
	j.pid = 0
	if !j.closed {
		j.state = "ready"
		if err != nil && !errors.Is(err, context.Canceled) {
			j.state = "failed"
			j.failure = errorCode(err)
		}
		changed(j)
		if j.failure == "" {
			for index := range j.demands {
				if index == -1 && j.init == nil || index >= 0 && !m.cache.has(j.id+"/"+strconv.Itoa(index)) {
					m.enqueue(j)
					break
				}
			}
		}
	}
}

func (m *Manager) StopEncoding(owner, id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.get(owner, id)
	if err != nil {
		return false
	}
	if replacement, err := m.get(owner, j.replacement); err == nil && replacement.identity.ItemID == j.identity.ItemID {
		m.end(j)
		return true
	}
	j.generation++
	if j.runCancel != nil {
		j.runCancel()
	}
	j.failure = ""
	j.lastUse = time.Now()
	changed(j)
	return true
}

// LinkReplacement records a newly negotiated stream for the same playback.
// The old plan remains usable until the client explicitly stops its encoding.
func (m *Manager) LinkReplacement(owner, previousID, nextID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	previous, e1 := m.get(owner, previousID)
	next, e2 := m.get(owner, nextID)
	if e1 != nil || e2 != nil || previous == next || previous.identity.ItemID != next.identity.ItemID {
		return
	}
	previous.replacement = nextID
	next.playbackID = previous.playbackID
}
func (m *Manager) End(owner, id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.get(owner, id)
	if err != nil {
		return false
	}
	m.end(j)
	return true
}
func (m *Manager) end(j *job) {
	j.closed = true
	j.state = "stopped"
	if j.failure != "" {
		j.state = "failed"
	}
	j.cancel()
	j.generation++
	changed(j)
	delete(m.jobs, j.id)
	delete(m.inputs, j.inputKey)
	m.cache.dropPrefix(j.id + "/")
	m.recent = append(m.recent, m.jobView(j, nil))
	if len(m.recent) > 100 {
		m.recent = append([]JobView(nil), m.recent[len(m.recent)-100:]...)
	}
}

func (m *Manager) EndOwner(owner string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		if j.identity.Owner == owner {
			m.end(j)
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
			jobs := sortedJobs(m.jobs)
			m.mu.Unlock()
			valid := map[string]bool{}
			for _, j := range jobs {
				if _, checked := valid[j.identity.Owner]; !checked {
					ctx, cancel := context.WithTimeout(m.ctx, 2*time.Second)
					valid[j.identity.Owner] = j.source.Valid == nil || j.source.Valid(ctx)
					cancel()
				}
				m.mu.Lock()
				if !j.closed && (!valid[j.identity.Owner] || now.Sub(j.lastUse) > m.cfg.IdleTTL) {
					m.end(j)
				}
				m.mu.Unlock()
			}
		}
	}
}
func (m *Manager) NotePlayback(owner, id string, position int64, paused *bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.get(owner, id)
	if err != nil {
		return
	}
	now := time.Now()
	j.lastUse = now
	j.positionAt = &now
	j.position = &position
	if paused != nil {
		value := *paused
		j.paused = &value
	}
}
func (m *Manager) HasActiveWork() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		if j.running || j.preparing {
			return true
		}
	}
	return false
}

func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	for _, j := range m.jobs {
		m.end(j)
	}
	m.cancel()
	m.mu.Unlock()
	_ = m.server.Close()
	m.wg.Wait()
	err := os.RemoveAll(m.cache.root)
	return errors.Join(err, m.lock.Close())
}

func (m *Manager) serveInput(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/input/")
	m.mu.Lock()
	j := m.inputs[key]
	if j == nil || j.closed || !j.running {
		m.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	source := j.source
	ctx := j.ctx
	m.mu.Unlock()
	start, end := int64(0), source.Media.Size-1
	partial := false
	if raw := r.Header.Get("Range"); raw != "" {
		if !strings.HasPrefix(raw, "bytes=") || strings.Contains(raw, ",") {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		first, last, ok := strings.Cut(strings.TrimPrefix(raw, "bytes="), "-")
		if !ok {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		var err error
		if first == "" {
			n, e := strconv.ParseInt(last, 10, 64)
			err = e
			if n <= 0 {
				err = ErrSource
			}
			start = max(0, source.Media.Size-n)
		} else {
			start, err = strconv.ParseInt(first, 10, 64)
			if last != "" {
				end, err = strconv.ParseInt(last, 10, 64)
			}
		}
		if err != nil || start < 0 || end < start || start >= source.Media.Size {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		end = min(end, source.Media.Size-1)
		partial = true
	}
	requestCtx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(ctx, cancel)
	defer func() { stop(); cancel() }()
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.Header().Set("Accept-Ranges", "bytes")
		return
	}
	body, err := source.Open(requestCtx, start, end-start+1)
	if err != nil {
		http.Error(w, "media source unavailable", http.StatusBadGateway)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, source.Media.Size))
		w.WriteHeader(http.StatusPartialContent)
	}
	if _, err = io.CopyN(w, body, end-start+1); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, ErrIndex):
		return "index_unavailable"
	case errors.Is(err, ErrSource):
		return "source_unavailable"
	case errors.Is(err, ErrCapacity):
		return "capacity_exhausted"
	case errors.Is(err, context.DeadlineExceeded):
		return "work_timeout"
	default:
		return "worker_failed"
	}
}
func codeError(code string) error {
	switch code {
	case "index_unavailable":
		return ErrIndex
	case "source_unavailable":
		return ErrSource
	case "capacity_exhausted":
		return ErrCapacity
	default:
		return ErrWorker
	}
}

func sortedJobs(jobs map[string]*job) []*job {
	out := make([]*job, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].created.Equal(out[j].created) {
			return out[i].id > out[j].id
		}
		return out[i].created.After(out[j].created)
	})
	return out
}
