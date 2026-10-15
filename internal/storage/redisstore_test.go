package storage

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRedisStoreGate проверяет предохранитель деградации: в окне после сбоя
// обращения к Redis нет, запросы сразу получают признак деградации, после
// окна попытка повторяется.
func TestRedisStoreGate(t *testing.T) {
	store, err := NewRedisStore("127.0.0.1:1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	_, _, err = store.Get(context.Background(), "key")
	require.Error(t, err, "первое обращение к недоступному Redis падает")
	require.NotErrorIs(t, err, ErrDegraded, "первая ошибка - не деградация")

	_, _, err = store.Get(context.Background(), "key")
	require.ErrorIs(t, err, ErrDegraded, "повтор в окне деградации не обращается к Redis")

	store.mu.Lock()
	store.degradedUntil = time.Now().Add(-time.Millisecond)
	store.mu.Unlock()
	_, _, err = store.Get(context.Background(), "key")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrDegraded, "после окна попытка повторяется")
}

// TestRedisStoreGateCanceled проверяет, что отмена клиента не взводит
// предохранитель (это не сбой зависимости).
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
	assert.False(t, degraded, "отмена клиента не взводит предохранитель")
}
