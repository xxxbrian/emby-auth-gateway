package transcode

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

type Aggregate struct {
	Enabled          bool   `json:"enabled"`
	FFmpegVersion    string `json:"ffmpeg_version"`
	WorkerLimit      int    `json:"worker_limit"`
	Running          int    `json:"running"`
	Queued           int    `json:"queued"`
	Ready            int    `json:"ready"`
	Preparing        int    `json:"preparing"`
	Failed           int    `json:"failed"`
	CacheBytes       int64  `json:"cache_bytes"`
	CacheBudgetBytes int64  `json:"cache_budget_bytes"`
	ReservedBytes    int64  `json:"reserved_bytes"`
	PinnedBytes      int64  `json:"pinned_bytes"`
	CacheHits        uint64 `json:"cache_hits"`
	CacheMisses      uint64 `json:"cache_misses"`
	CacheEvictions   uint64 `json:"cache_evictions"`
}
type Interval struct {
	StartTicks int64 `json:"start_ticks"`
	EndTicks   int64 `json:"end_ticks"`
}
type JobView struct {
	SourceRef           string     `json:"source_ref,omitempty"`
	PlaybackID          string     `json:"playback_id"`
	ID                  string     `json:"id"`
	UserID              string     `json:"user_id"`
	Username            string     `json:"username"`
	Device              string     `json:"device"`
	ItemID              string     `json:"item_id"`
	ItemName            string     `json:"item_name"`
	SourceName          string     `json:"source_name"`
	AudioSource         string     `json:"audio_source"`
	AudioSourceChannels int        `json:"audio_source_channels"`
	AudioOutput         string     `json:"audio_output"`
	AudioOutputChannels int        `json:"audio_output_channels"`
	VideoCodec          string     `json:"video_codec"`
	VideoMode           string     `json:"video_mode"`
	Reason              string     `json:"reason"`
	State               string     `json:"state"`
	Failure             string     `json:"failure"`
	PositionTicks       *int64     `json:"position_ticks"`
	PositionAt          *time.Time `json:"position_at"`
	Paused              *bool      `json:"paused"`
	RequestedTicks      int64      `json:"requested_ticks"`
	ProducedTicks       int64      `json:"produced_ticks"`
	DurationTicks       int64      `json:"duration_ticks"`
	Runs                int        `json:"runs"`
	Speed               *float64   `json:"speed"`
	RSSBytes            *int64     `json:"rss_bytes"`
	CPUPercent          *float64   `json:"cpu_percent"` // 100% = one CPU core
	CacheBytes          int64      `json:"cache_bytes"`
	CachedSegments      int        `json:"cached_segments"`
	Ranges              []Interval `json:"ranges"`
	RangesTruncated     bool       `json:"ranges_truncated"`
	CreatedAt           time.Time  `json:"created_at"`
	LastSeen            time.Time  `json:"last_seen"`
}
type Observation struct {
	BootID    string    `json:"boot_id"`
	At        time.Time `json:"at"`
	Aggregate Aggregate `json:"aggregate"`
	Jobs      []JobView `json:"jobs"`
	Recent    []JobView `json:"recent"`
}

// Snapshot is a read of an immutable observation. It performs no source,
// process, cache-directory, or task-control operation.
func (m *Manager) Snapshot() Observation {
	if m == nil {
		return Observation{Jobs: []JobView{}, Recent: []JobView{}}
	}
	o := m.observation.Load()
	if o == nil {
		return Observation{Jobs: []JobView{}, Recent: []JobView{}}
	}
	return *o
}
func label(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

func (m *Manager) jobView(j *job, entries map[string]int64) JobView {
	view := JobView{ID: j.id, UserID: j.identity.UserID, Username: label(j.identity.Username, 128), Device: label(j.identity.Device, 128), ItemID: j.identity.ItemID, ItemName: label(j.identity.ItemName, 256), SourceName: label(j.source.Media.Name, 256), AudioSource: j.plan.Audio.Codec, AudioSourceChannels: j.plan.Audio.Channels, AudioOutput: j.plan.AudioCodec, AudioOutputChannels: j.plan.AudioChannels, VideoCodec: j.plan.Video.Codec, VideoMode: "copy", Reason: j.plan.Reason, State: j.state, Failure: j.failure, PositionTicks: j.position, PositionAt: j.positionAt, Paused: j.paused, RequestedTicks: j.requested, ProducedTicks: j.produced, DurationTicks: j.source.Media.RunTimeTicks, Runs: j.runs, Ranges: []Interval{}, CreatedAt: j.created, LastSeen: j.lastUse}
	if view.AudioOutput == "copy" {
		view.AudioOutput = j.plan.Audio.Codec
	}
	view.PlaybackID = j.playbackID
	view.SourceRef = j.identity.SourceRef
	if j.running && j.produced > j.runStartTicks {
		speed := float64(j.produced-j.runStartTicks) / float64(TicksPerSecond) / time.Since(j.runStarted).Seconds()
		view.Speed = &speed
	}
	var indices []int
	for key, size := range entries {
		index, err := strconv.Atoi(key)
		if err != nil || index < 0 || index >= j.timeline.Len() {
			continue
		}
		indices = append(indices, index)
		view.CacheBytes += size
	}
	sort.Ints(indices)
	view.CachedSegments = len(indices)
	for _, index := range indices {
		r := Interval{j.timeline.Cuts[index], j.timeline.Cuts[index+1]}
		if n := len(view.Ranges); n > 0 && view.Ranges[n-1].EndTicks == r.StartTicks {
			view.Ranges[n-1].EndTicks = r.EndTicks
		} else if len(view.Ranges) < 64 {
			view.Ranges = append(view.Ranges, r)
		} else {
			view.RangesTruncated = true
		}
	}
	return view
}

func (m *Manager) publishObservation() {
	m.mu.Lock()
	defer m.mu.Unlock()
	used, reserved, pinned, hits, misses, evictions := m.cache.stats()
	o := Observation{BootID: m.cfg.BootID, At: time.Now().UTC(), Aggregate: Aggregate{Enabled: true, FFmpegVersion: m.version, WorkerLimit: m.cfg.Workers, CacheBytes: used, CacheBudgetBytes: m.cfg.CacheBytes, ReservedBytes: reserved, PinnedBytes: pinned, CacheHits: hits, CacheMisses: misses, CacheEvictions: evictions}, Jobs: []JobView{}, Recent: append([]JobView{}, m.recent...)}
	grouped := map[string]map[string]int64{}
	for key, size := range m.cache.keys("") {
		id, index, ok := strings.Cut(key, "/")
		if !ok {
			continue
		}
		if grouped[id] == nil {
			grouped[id] = map[string]int64{}
		}
		grouped[id][index] = size
	}
	for _, j := range sortedJobs(m.jobs) {
		view := m.jobView(j, grouped[j.id])
		o.Jobs = append(o.Jobs, view)
		switch j.state {
		case "preparing":
			o.Aggregate.Preparing++
		case "queued":
			o.Aggregate.Queued++
		case "producing":
			o.Aggregate.Running++
		case "failed":
			o.Aggregate.Failed++
		default:
			o.Aggregate.Ready++
		}
	}
	m.observation.Store(&o)
}

func (m *Manager) observe() {
	defer m.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	previous := map[int]processSample{}
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-ticker.C:
			m.mu.Lock()
			pids := map[string]int{}
			for _, j := range m.jobs {
				if j.pid > 0 {
					pids[j.id] = j.pid
				}
			}
			m.mu.Unlock()
			metrics := map[string]processMetrics{}
			next := map[int]processSample{}
			for id, pid := range pids {
				sample := sampleProcess(pid)
				sample.At = now
				metric := processMetrics{RSS: sample.RSS}
				if sample.CPU != nil {
					if prev := previous[pid]; prev.CPU != nil && *sample.CPU >= *prev.CPU {
						v := (*sample.CPU - *prev.CPU) / now.Sub(prev.At).Seconds() * 100
						metric.CPU = &v
					}
				}
				metrics[id] = metric
				next[pid] = sample
			}
			previous = next
			m.publishObservation()
			current := m.Snapshot()
			o := current
			o.Jobs = append([]JobView(nil), current.Jobs...)
			for i := range o.Jobs {
				metric := metrics[o.Jobs[i].ID]
				o.Jobs[i].RSSBytes, o.Jobs[i].CPUPercent = metric.RSS, metric.CPU
			}
			m.observation.Store(&o)
		}
	}
}

type processMetrics struct {
	RSS *int64
	CPU *float64
}
type processSample struct {
	RSS *int64
	CPU *float64
	At  time.Time
}
