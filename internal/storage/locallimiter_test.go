package storage

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLocalLimiterIncr проверяет инкремент и независимость ключей.
func TestLocalLimiterIncr(t *testing.T) {
	limiter := NewLocalLimiter()
	ctx := context.Background()
	for expected := int64(1); expected <= 3; expected++ {
		count, err := limiter.Incr(ctx, "rl:a:route:1", time.Minute)
		require.NoError(t, err)
		assert.Equal(t, expected, count)
	}
	count, err := limiter.Incr(ctx, "rl:b:route:1", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "разные ключи считаются независимо")
}

// TestLocalLimiterExpiry проверяет переиспользование истекшей записи.
func TestLocalLimiterExpiry(t *testing.T) {
	limiter := NewLocalLimiter()
	ctx := context.Background()
	first, err := limiter.Incr(ctx, "rl:a:route:1", 20*time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, int64(1), first)
	time.Sleep(30 * time.Millisecond)
	second, err := limiter.Incr(ctx, "rl:a:route:1", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, int64(1), second, "истекшее окно начинается с нуля")
}

// TestLocalLimiterSweep проверяет уборку истекших записей: периодический
// вызов sweep не дает карте расти при потоке уникальных ключей.
func TestLocalLimiterSweep(t *testing.T) {
	limiter := NewLocalLimiter()
	now := time.Now()
	limiter.counts["stale"] = localWindow{count: 1, expiresAt: now.Add(-time.Second)}
	limiter.counts["live"] = localWindow{count: 1, expiresAt: now.Add(time.Minute)}
	limiter.sweep(now)
	assert.NotContains(t, limiter.counts, "stale")
	assert.Contains(t, limiter.counts, "live")
}

// TestLocalLimiterConcurrent проверяет атомарность инкрементов.
func TestLocalLimiterConcurrent(t *testing.T) {
	limiter := NewLocalLimiter()
	ctx := context.Background()
	var waitGroup sync.WaitGroup
	for worker := 0; worker < 50; worker++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for i := 0; i < 20; i++ {
				_, err := limiter.Incr(ctx, "rl:shared:route:1", time.Minute)
				assert.NoError(t, err)
			}
		}()
	}
	waitGroup.Wait()
	count, err := limiter.Incr(ctx, "rl:shared:route:1", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, int64(50*20+1), count)
}
