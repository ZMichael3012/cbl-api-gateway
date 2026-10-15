package usecase

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"
)

// NewFallbackCounter собирает счетчик с fallback: при ошибке основного
// (Redis) используется локальный счетчик инстанса (T-11: потеря Redis
// деградирует точность лимита, а не отказывает в обслуживании). Журнал
// деградации троттлится: при высоком RPS ошибка зависимости не превращается
// в поток записей.
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

// fallbackCounter переключает счет на локальный при отказе основного.
type fallbackCounter struct {
	primary  Counter
	fallback Counter
	log      *zap.Logger

	mu       sync.Mutex
	lastWarn time.Time
}

// Incr пробует основной счетчик и при ошибке считает локально.
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

// warnDegraded пишет журнал деградации не чаще degradationLogInterval.
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
