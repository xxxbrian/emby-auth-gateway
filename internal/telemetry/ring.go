package telemetry

import "time"

// counters are additive metrics for a single time bucket.
type counters struct {
	Requests      int64
	BytesIn       int64
	BytesOut      int64
	Errors        int64
	Rejects       int64
	UserdataFails int64
	OverlayFails  int64
	PlaybacksMax  int64 // gauge sample: max active playbacks observed in bucket
}

func (c *counters) add(o counters) {
	c.Requests += o.Requests
	c.BytesIn += o.BytesIn
	c.BytesOut += o.BytesOut
	c.Errors += o.Errors
	c.Rejects += o.Rejects
	c.UserdataFails += o.UserdataFails
	c.OverlayFails += o.OverlayFails
	if o.PlaybacksMax > c.PlaybacksMax {
		c.PlaybacksMax = o.PlaybacksMax
	}
}

// timeRing is a fixed-size ring of counters indexed by absolute time units.
type timeRing struct {
	interval time.Duration
	size     int
	slots    []counters
	headUnix int64 // unit index of the current head slot
	headIdx  int
	inited   bool
}

func newTimeRing(interval time.Duration, size int) *timeRing {
	return &timeRing{
		interval: interval,
		size:     size,
		slots:    make([]counters, size),
	}
}

func (r *timeRing) unit(t time.Time) int64 {
	ns := t.UnixNano()
	if ns < 0 {
		ns = 0
	}
	return ns / int64(r.interval)
}

func (r *timeRing) advanceTo(unit int64) {
	if !r.inited {
		r.headUnix = unit
		r.headIdx = 0
		r.inited = true
		return
	}
	if unit <= r.headUnix {
		return
	}
	delta := unit - r.headUnix
	if delta >= int64(r.size) {
		for i := range r.slots {
			r.slots[i] = counters{}
		}
		r.headUnix = unit
		r.headIdx = 0
		return
	}
	for i := int64(0); i < delta; i++ {
		r.headIdx = (r.headIdx + 1) % r.size
		r.slots[r.headIdx] = counters{}
		r.headUnix++
	}
}

func (r *timeRing) slotIndex(unit int64) (int, bool) {
	if !r.inited {
		return 0, false
	}
	age := r.headUnix - unit
	if age < 0 || age >= int64(r.size) {
		return 0, false
	}
	idx := r.headIdx - int(age)
	if idx < 0 {
		idx += r.size
	}
	return idx, true
}

func (r *timeRing) add(t time.Time, fn func(*counters)) {
	u := r.unit(t)
	r.advanceTo(u)
	idx, ok := r.slotIndex(u)
	if !ok {
		return
	}
	fn(&r.slots[idx])
}

func (r *timeRing) sum(now time.Time, windowUnits int64) counters {
	u := r.unit(now)
	r.advanceTo(u)
	if windowUnits <= 0 {
		return counters{}
	}
	if windowUnits > int64(r.size) {
		windowUnits = int64(r.size)
	}
	var out counters
	for age := int64(0); age < windowUnits; age++ {
		idx := r.headIdx - int(age)
		if idx < 0 {
			idx += r.size
		}
		out.add(r.slots[idx])
	}
	return out
}

// series returns the last n points (oldest first) as (time, value) using extract.
func (r *timeRing) series(now time.Time, n int, extract func(counters) float64) []SeriesPoint {
	u := r.unit(now)
	r.advanceTo(u)
	if n <= 0 {
		return nil
	}
	if n > r.size {
		n = r.size
	}
	out := make([]SeriesPoint, 0, n)
	// oldest first: age = n-1 .. 0
	for age := n - 1; age >= 0; age-- {
		idx := r.headIdx - age
		if idx < 0 {
			idx += r.size
		}
		slotUnix := r.headUnix - int64(age)
		ts := time.Unix(0, slotUnix*int64(r.interval)).UTC()
		out = append(out, SeriesPoint{T: ts, V: extract(r.slots[idx])})
	}
	return out
}
