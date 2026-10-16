package storage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// degradeWindow sets the window of the local circuit breaker: after the first
// Redis error, requests do not call Redis at all for degradeWindow and go
// straight to the local limiter. Without the breaker every request would pay
// client timeouts (up to ~1 s per operation), and at 3000 RPS that is
// thousands of goroutines.
const degradeWindow = time.Second

// ErrDegraded means Redis is marked unavailable by the circuit breaker.
var ErrDegraded = errors.New("redis is degraded")

// redisIncrScript atomically increments the window counter and sets a TTL if
// there is none (the key could be created externally without a TTL): the
// limit decision keeps its atomicity and counters do not leak.
var redisIncrScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if redis.call('PTTL', KEYS[1]) < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return count`)

// RedisStore serves the perimeter Redis: the key check cache and rate limit
// counters.
type RedisStore struct {
	client *redis.Client

	mu            sync.Mutex
	degradedUntil time.Time
}

// NewRedisStore creates a client with short hot path timeouts: the first
// Redis error costs up to ~1 s (two attempts with 200/300 ms timeouts),
// after which the circuit breaker for degradeWindow switches requests to the
// local limiter.
func NewRedisStore(addr string) (*RedisStore, error) {
	client := redis.NewClient(&redis.Options{
		Addr:                  addr,
		DialTimeout:           200 * time.Millisecond,
		ReadTimeout:           300 * time.Millisecond,
		WriteTimeout:          300 * time.Millisecond,
		MaxRetries:            1,
		ContextTimeoutEnabled: true,
	})
	return &RedisStore{client: client}, nil
}

// Ping checks Redis availability.
func (s *RedisStore) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}

// Close closes the Redis connection.
func (s *RedisStore) Close() error {
	return s.client.Close()
}

// Get reads the key value; found is false on a miss.
func (s *RedisStore) Get(ctx context.Context, key string) (string, bool, error) {
	if err := s.gate(); err != nil {
		return "", false, err
	}
	value, err := s.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		s.trip(ctx, err)
		return "", false, err
	}
	return value, true, nil
}

// Incr atomically increments the window counter and returns the value after
// the increment; a TTL is set if there is none so counters do not leak.
func (s *RedisStore) Incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	if err := s.gate(); err != nil {
		return 0, err
	}
	milliseconds := ttl.Milliseconds()
	if milliseconds < 1 {
		milliseconds = 1
	}
	count, err := redisIncrScript.Run(ctx, s.client, []string{key}, milliseconds).Int64()
	if err != nil {
		s.trip(ctx, err)
		return 0, fmt.Errorf("incr window: %w", err)
	}
	return count, nil
}

// gate checks the circuit breaker: no Redis calls are made within the
// degradation window.
func (s *RedisStore) gate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().Before(s.degradedUntil) {
		return ErrDegraded
	}
	return nil
}

// trip marks Redis unavailable for degradeWindow; a client cancellation is
// not considered a dependency failure and does not trip the breaker.
func (s *RedisStore) trip(ctx context.Context, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	s.mu.Lock()
	s.degradedUntil = time.Now().Add(degradeWindow)
	s.mu.Unlock()
}
