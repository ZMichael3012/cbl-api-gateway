package storage

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRedisStoreGate checks the degradation circuit breaker: within the
// window after a failure there are no Redis calls and requests immediately
// get the degradation flag; after the window the attempt is repeated.
func TestRedisStoreGate(t *testing.T) {
	store, err := NewRedisStore("127.0.0.1:1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	_, _, err = store.Get(context.Background(), "key")
	require.Error(t, err, "the first call to an unavailable Redis fails")
	require.NotErrorIs(t, err, ErrDegraded, "the first error is not degradation")

	_, _, err = store.Get(context.Background(), "key")
	require.ErrorIs(t, err, ErrDegraded, "a repeat within the degradation window does not call Redis")

	store.mu.Lock()
	store.degradedUntil = time.Now().Add(-time.Millisecond)
	store.mu.Unlock()
	_, _, err = store.Get(context.Background(), "key")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrDegraded, "after the window the attempt is repeated")
}

// TestRedisStoreGateCanceled checks that a client cancellation does not trip
// the breaker (it is not a dependency failure).
func TestRedisStoreGateCanceled(t *testing.T) {
	store, err := NewRedisStore("127.0.0.1:1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = store.Get(ctx, "key")
	require.Error(t, err)
	store.mu.Lock()
	degraded := !store.degradedUntil.IsZero()
	store.mu.Unlock()
	assert.False(t, degraded, "a client cancellation does not trip the breaker")
}
