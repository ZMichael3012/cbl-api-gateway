package storage

import (
	"context"
	"sync"
	"time"
)

// sweepInterval sets how often expired entries are swept (every N increments)
// so the map does not grow indefinitely under a stream of unique keys.
const sweepInterval = 1024

// LocalLimiter counts windows in process memory: the fallback when Redis is
// unavailable and the local mode without REDIS_ADDR; with several replicas
// the effective limit is multiplied by their number (perimeter trade-off).
type LocalLimiter struct {
	mu     sync.Mutex
	counts map[string]localWindow
	ops    int
}

// localWindow stores the counter and the expiration moment of a window entry.
type localWindow struct {
	count     int64
	expiresAt time.Time
}

// NewLocalLimiter creates a local limiter.
func NewLocalLimiter() *LocalLimiter {
	return &LocalLimiter{counts: make(map[string]localWindow)}
}

// Incr increments the counter of a key that lives until ttl (for window keys
// the ttl covers the window with a margin); an expired entry is reused from
// zero.
func (l *LocalLimiter) Incr(_ context.Context, key string, ttl time.Duration) (int64, error) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.counts[key]
	if !ok || !entry.expiresAt.After(now) {
		entry = localWindow{expiresAt: now.Add(ttl)}
	}
	entry.count++
	l.counts[key] = entry
	l.ops++
	if l.ops%sweepInterval == 0 {
		l.sweep(now)
	}
	return entry.count, nil
}

// sweep deletes expired entries; it is called rarely, so its cost is
// amortized.
func (l *LocalLimiter) sweep(now time.Time) {
	for key, entry := range l.counts {
		if !entry.expiresAt.After(now) {
			delete(l.counts, key)
		}
	}
}
