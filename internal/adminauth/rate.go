package adminauth

import (
	"sync"
	"time"
)

// RateLimiter is a simple fixed-window counter limiter.
type RateLimiter struct {
	mu       sync.Mutex
	windows  map[string]*rateWindow
	limit    int
	window   time.Duration
	now      func() time.Time
}

type rateWindow struct {
	count int
	start time.Time
}

// NewRateLimiter creates a limiter allowing limit events per window.
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	if limit <= 0 {
		limit = 20
	}
	if window <= 0 {
		window = time.Minute
	}
	return &RateLimiter{
		windows: make(map[string]*rateWindow),
		limit:   limit,
		window:  window,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// Allow reports whether key may proceed and records the attempt when allowed.
func (r *RateLimiter) Allow(key string) bool {
	if r == nil {
		return true
	}
	if key == "" {
		key = "_"
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()

	// Opportunistic cleanup of stale windows.
	if len(r.windows) > 10_000 {
		for k, w := range r.windows {
			if now.Sub(w.start) > r.window*2 {
				delete(r.windows, k)
			}
		}
	}

	w := r.windows[key]
	if w == nil || now.Sub(w.start) >= r.window {
		r.windows[key] = &rateWindow{count: 1, start: now}
		return true
	}
	if w.count >= r.limit {
		return false
	}
	w.count++
	return true
}
