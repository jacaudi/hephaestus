package boot

import (
	"maps"
	"sync"
	"time"
)

// keyWindow is one key's budget: hits since start.
type keyWindow struct {
	start time.Time
	hits  int
}

// windowLimiter allows a key at most limit hits per fixed window, which starts
// at the key's first hit. It tracks at most capacity keys, so a flood of
// distinct keys, such as source addresses, cannot grow it without bound.
type windowLimiter[K comparable] struct {
	limit    int
	window   time.Duration
	capacity int
	now      func() time.Time

	mu   sync.Mutex
	keys map[K]keyWindow
}

// newWindowLimiter returns a limiter that allows limit hits per key per window
// and tracks at most capacity keys. now is its clock.
func newWindowLimiter[K comparable](limit int, window time.Duration, capacity int, now func() time.Time) *windowLimiter[K] {
	return &windowLimiter[K]{limit: limit, window: window, capacity: capacity, now: now, keys: make(map[K]keyWindow)}
}

// Allow spends one of key's hits and reports whether it had one. At capacity a
// new key first sweeps the expired windows, and is refused if the table is
// still full. It is safe for concurrent use.
func (l *windowLimiter[K]) Allow(key K) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	w, ok := l.keys[key]
	if !ok && len(l.keys) >= l.capacity {
		maps.DeleteFunc(l.keys, func(_ K, w keyWindow) bool { return l.expired(w, now) })
		if len(l.keys) >= l.capacity {
			return false
		}
	}
	if !ok || l.expired(w, now) {
		w = keyWindow{start: now}
	}
	allowed := w.hits < l.limit
	if allowed {
		w.hits++
	}
	l.keys[key] = w
	return allowed
}

// expired reports whether w ended before now.
func (l *windowLimiter[K]) expired(w keyWindow, now time.Time) bool {
	return now.Sub(w.start) >= l.window
}
