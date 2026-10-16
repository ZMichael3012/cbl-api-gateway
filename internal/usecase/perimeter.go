package usecase

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/cbl/cbl-api-gateway/internal/domain"
)

// degradationLogInterval sets the degradation log rate: at 3000 RPS a record
// per request would turn logs into a stream, so a dependency failure is
// recorded at most once per interval.
const degradationLogInterval = 5 * time.Second

// Counter counts requests within a limit window (Redis or local fallback).
type Counter interface {
	// Incr increments the window key counter and returns the post-increment value.
	Incr(ctx context.Context, key string, ttl time.Duration) (int64, error)
}

// KeyPeeker reads the API key check cache apikey:v1:* populated by the
// Billing API (contract 7.2 FR-6).
type KeyPeeker interface {
	// Get returns the key value; found is false on a miss.
	Get(ctx context.Context, key string) (string, bool, error)
}

// KeyCheck describes the outcome of API key control at the perimeter.
type KeyCheck struct {
	// Deny rejects the request without calling the Billing API (revocation tombstone).
	Deny bool
	// MerchantID holds the key owner when the cache confirms the key.
	MerchantID string
}

// LimitDecision describes a request limit decision.
type LimitDecision struct {
	// Allowed permits proxying the request.
	Allowed bool
	// RetryAfter sets the remaining window for the Retry-After header on rejection.
	RetryAfter time.Duration
}

// Perimeter applies the perimeter rules: API key control via the Billing API
// cache and a per-merchant limit within a fixed-length window.
type Perimeter struct {
	keys    KeyPeeker
	counter Counter
	log     *zap.Logger
	limit   int64
	window  time.Duration

	logMu    sync.Mutex
	lastWarn time.Time
}

// NewPerimeter builds the perimeter rules; the limit and the window must be
// positive (invalid configuration fails startup instead of silently passing).
// The CBL-003 contract key window is one second (the value is set by main).
func NewPerimeter(keys KeyPeeker, counter Counter, log *zap.Logger, limit int64, window time.Duration) (*Perimeter, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("rate limit must be positive: %d", limit)
	}
	if window <= 0 {
		return nil, fmt.Errorf("rate window must be positive: %s", window)
	}
	if counter == nil {
		return nil, errors.New("counter is required")
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Perimeter{keys: keys, counter: counter, log: log, limit: limit, window: window}, nil
}

// CheckKey reads the check cache: deny rejects the request, ok confirms the
// merchant, and a miss, a Redis error or a corrupted value delegates the
// decision to the Billing API (the perimeter does not make authoritative
// decisions about an unknown key).
func (p *Perimeter) CheckKey(ctx context.Context, tokenHash string) KeyCheck {
	if p.keys == nil {
		return KeyCheck{}
	}
	value, present, err := p.keys.Get(ctx, domain.APIKeyCacheKey(tokenHash))
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// The client aborted the request: this is not a dependency failure.
			return KeyCheck{}
		}
		p.warnDegraded("api key cache read failed, request is delegated to billing api", err)
		return KeyCheck{}
	}
	status, merchantID := domain.ParseKeyCacheValue(value, present)
	switch status {
	case domain.KeyAllowed:
		return KeyCheck{MerchantID: merchantID}
	case domain.KeyDenied:
		return KeyCheck{Deny: true}
	case domain.KeyInvalid:
		p.warnDegraded("api key cache value is invalid, request is delegated to billing api", nil)
	}
	return KeyCheck{}
}

// Allow counts the request within the fixed-length window and decides whether
// to pass it; when both counters are unavailable the request is passed
// (perimeter availability is more important, the authoritative key check
// remains with the Billing API).
func (p *Perimeter) Allow(ctx context.Context, identity, route string, now time.Time) LimitDecision {
	windowIndex := now.UnixNano() / int64(p.window)
	key := domain.RateLimitKey(identity, route, windowIndex)
	count, err := p.counter.Incr(ctx, key, 2*p.window)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return LimitDecision{Allowed: true}
		}
		p.warnDegraded("rate counter failed, request is allowed without limit", err)
		return LimitDecision{Allowed: true}
	}
	if count <= p.limit {
		return LimitDecision{Allowed: true}
	}
	windowEnd := time.Unix(0, (windowIndex+1)*int64(p.window))
	return LimitDecision{Allowed: false, RetryAfter: windowEnd.Sub(now)}
}

// warnDegraded logs degradation at most once per degradationLogInterval.
func (p *Perimeter) warnDegraded(message string, err error) {
	now := time.Now()
	p.logMu.Lock()
	skip := now.Sub(p.lastWarn) < degradationLogInterval
	if !skip {
		p.lastWarn = now
	}
	p.logMu.Unlock()
	if skip {
		return
	}
	if err != nil {
		p.log.Warn(message, zap.Error(err))
		return
	}
	p.log.Warn(message)
}
