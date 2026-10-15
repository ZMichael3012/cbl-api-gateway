//go:build integration

package storage_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/cbl/cbl-api-gateway/internal/storage"
)

// startRedis поднимает Redis 7 в testcontainers и возвращает адрес.
func startRedis(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:7-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForListeningPort("6379/tcp").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "6379/tcp")
	require.NoError(t, err)
	return fmt.Sprintf("%s:%s", host, port.Port())
}

// newClient собирает клиент для проверки состояния Redis в тестах.
func newClient(t *testing.T, addr string) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestRedisStoreGet проверяет чтение значений кэша проверки ключей.
func TestRedisStoreGet(t *testing.T) {
	ctx := context.Background()
	addr := startRedis(t)
	client := newClient(t, addr)
	store, err := storage.NewRedisStore(addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	require.NoError(t, store.Ping(ctx))

	require.NoError(t, client.Set(ctx, "apikey:v1:abc", "ok:01912f2f-6f82-7d5e-8e04-4d4b5b6a1234", time.Minute).Err())
	value, found, err := store.Get(ctx, "apikey:v1:abc")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "ok:01912f2f-6f82-7d5e-8e04-4d4b5b6a1234", value)

	_, found, err = store.Get(ctx, "apikey:v1:missing")
	require.NoError(t, err)
	assert.False(t, found, "промах - не ошибка")
}

// TestRedisStoreIncrSetsTTLForExternalKey проверяет, что ключ, созданный
// извне без TTL, получает TTL первым инкрементом (иначе счетчик не истечет).
func TestRedisStoreIncrSetsTTLForExternalKey(t *testing.T) {
	ctx := context.Background()
	addr := startRedis(t)
	client := newClient(t, addr)
	store, err := storage.NewRedisStore(addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	require.NoError(t, client.Set(ctx, "rl:m:invoices:1700000001", "41", 0).Err())
	ttl, err := client.TTL(ctx, "rl:m:invoices:1700000001").Result()
	require.NoError(t, err)
	require.Less(t, ttl, time.Duration(0), "ключ без TTL")

	count, err := store.Incr(ctx, "rl:m:invoices:1700000001", 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, int64(42), count)
	ttl, err = client.PTTL(ctx, "rl:m:invoices:1700000001").Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0), "TTL выставлен предохранительно")
}

// TestRedisStoreIncrAtomic проверяет атомарность счетчика при параллельных
// инкрементах (атака параллельными запросами не обходит лимит).
func TestRedisStoreIncrAtomic(t *testing.T) {
	ctx := context.Background()
	addr := startRedis(t)
	client := newClient(t, addr)
	store, err := storage.NewRedisStore(addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	const workers = 50
	const perWorker = 20
	var waitGroup sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for i := 0; i < perWorker; i++ {
				_, incrErr := store.Incr(ctx, "rl:m:invoices:1700000000", 2*time.Second)
				assert.NoError(t, incrErr)
			}
		}()
	}
	waitGroup.Wait()

	count, err := store.Incr(ctx, "rl:m:invoices:1700000000", 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, int64(workers*perWorker+1), count)
	ttl, err := client.PTTL(ctx, "rl:m:invoices:1700000000").Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0), "TTL выставлен")
	assert.LessOrEqual(t, ttl, 2*time.Second)
}
