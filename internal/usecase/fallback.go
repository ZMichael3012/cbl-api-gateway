package usecase

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"
)

// NewFallbackCounter builds a counter with a fallback: when the primary
// (Redis) counter fails, the local instance counter is used (T-11: losing
// Redis degrades limit accuracy instead of denying service). Degradation
// logging is throttled: at high RPS a dependency failure does not turn into
// a stream of records.
func NewFallbackCounter(primary, fallback Counter, log *zap.Logger) Counter {
	if log == nil {
		log = zap.NewNop()
	}
	if primary == nil {
		return fallback
	}
	if fallback == nil {
		return primary
	}
	return &fallbackCounter{primary: primary, fallback: fallback, log: log}
}

// fallbackCounter switches counting to the local counter when the primary fails.
type fallbackCounter struct {
	primary  Counter
	fallback Counter
	log      *zap.Logger

	mu       sync.Mutex
	lastWarn time.Time
}

// Incr tries the primary counter and counts locally on failure.
func (c *fallbackCounter) Incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	count, err := c.primary.Incr(ctx, key, ttl)
	if err == nil {
		return count, nil
	}
	if !errors.Is(err, context.Canceled) {
		c.warnDegraded(err)
	}
	return c.fallback.Incr(ctx, key, ttl)
}

// warnDegraded logs degradation at most once per degradationLogInterval.
func (c *fallbackCounter) warnDegraded(err error) {
	now := time.Now()
	c.mu.Lock()
	skip := now.Sub(c.lastWarn) < degradationLogInterval
	if !skip {
		c.lastWarn = now
	}
	c.mu.Unlock()
	if !skip {
		c.log.Warn("rate counter degraded to local limiter", zap.Error(err))
	}
}
