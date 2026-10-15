//go:build integration

package transport_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/cbl/cbl-kit/ginmw"

	"github.com/cbl/cbl-api-gateway/internal/domain"
	"github.com/cbl/cbl-api-gateway/internal/storage"
	"github.com/cbl/cbl-api-gateway/internal/transport"
	"github.com/cbl/cbl-api-gateway/internal/usecase"
)

// startRedisIntegration поднимает Redis 7 в testcontainers и возвращает клиент
// и адрес для сервиса периметра.
func startRedisIntegration(t *testing.T) (*redis.Client, string) {
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
	addr := fmt.Sprintf("%s:%s", host, port.Port())
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })
	return client, addr
}

// newIntegrationRouter собирает периметр на реальном Redis и stub-апстриме.
func newIntegrationRouter(t *testing.T, addr string, limit int64) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store, err := storage.NewRedisStore(addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	require.NoError(t, store.Ping(context.Background()))

	perimeter, err := usecase.NewPerimeter(store, usecase.NewFallbackCounter(store, storage.NewLocalLimiter(), nil), nil, limit, time.Second)
	require.NoError(t, err)
	backend := newBackend(t)
	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)
	router, err := transport.NewRouter(transport.Config{
		Perimeter:       perimeter,
		BillingAPI:      backendURL,
		Backoffice:      backendURL,
		UpstreamTimeout: 5 * time.Second,
	})
	require.NoError(t, err)
	return router
}

// TestPerimeterChainIntegration проверяет цепочку периметра на реальном Redis:
// подтверждение ключа, лимит, tombstone и делегирование промаха.
func TestPerimeterChainIntegration(t *testing.T) {
	ctx := context.Background()
	client, addr := startRedisIntegration(t)
	router := newIntegrationRouter(t, addr, 2)

	merchantID := "01912f2f-6f82-7d5e-8e04-4d4b5b6a1234"
	allowedToken := "cbl_v1_0123456789_allowed"
	allowedHash := domain.TokenHash(allowedToken)
	require.NoError(t, client.Set(ctx, domain.APIKeyCacheKey(allowedHash), "ok:"+merchantID, time.Minute).Err())

	// Всплеск запросов: все проходят в апстрим, сверхлимитные отклоняются 429
	// (границы окна не позволяют зафиксировать точный номер отказа).
	authorization := "Bearer " + allowedToken
	statuses := make([]int, 0, 10)
	for request := 0; request < 10; request++ {
		statuses = append(statuses, doRouter(router, http.MethodGet, "/v1/invoices", authorization).Code)
	}
	allowed, limited := 0, 0
	for _, status := range statuses {
		switch status {
		case http.StatusOK:
			allowed++
		case http.StatusTooManyRequests:
			limited++
		default:
			t.Fatalf("неожиданный статус: %d", status)
		}
	}
	assert.GreaterOrEqual(t, allowed, 2, "в пределах лимита запросы проксируются")
	assert.GreaterOrEqual(t, limited, 1, "сверхлимитные запросы отклоняются")
	if limited > 0 {
		denied := doRouter(router, http.MethodGet, "/v1/invoices", authorization)
		if denied.Code == http.StatusTooManyRequests {
			assert.Equal(t, "1", denied.Header().Get("Retry-After"))
			var problem ginmw.Problem
			require.NoError(t, json.Unmarshal(denied.Body.Bytes(), &problem))
			assert.Equal(t, "rate_limited", problem.Code)
		} else {
			assert.Equal(t, http.StatusOK, denied.Code, "граница окна могла открыть счет заново")
		}
	}

	// Счетчик живет в Redis с TTL; на границе секунды ключей окна может быть два.
	keys, err := client.Keys(ctx, "rl:"+merchantID+":invoices:*").Result()
	require.NoError(t, err)
	require.NotEmpty(t, keys)
	assert.LessOrEqual(t, len(keys), 2)
	ttl, err := client.PTTL(ctx, keys[0]).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0))

	// Tombstone отозванного ключа отсекается без вызова апстрима.
	deniedToken := "cbl_v1_0123456789_revoked"
	deniedHash := domain.TokenHash(deniedToken)
	require.NoError(t, client.Set(ctx, domain.APIKeyCacheKey(deniedHash), "deny", time.Minute).Err())
	response := doRouter(router, http.MethodGet, "/v1/invoices", "Bearer "+deniedToken)
	require.Equal(t, http.StatusUnauthorized, response.Code)

	// Промах кэша делегируется BFF и лимитируется по токену.
	missToken := "cbl_v1_0123456789_cold"
	miss := doRouter(router, http.MethodGet, "/v1/invoices", "Bearer "+missToken)
	require.Equal(t, http.StatusOK, miss.Code)
	keys, err = client.Keys(ctx, "rl:token:"+domain.TokenHash(missToken)+":invoices:*").Result()
	require.NoError(t, err)
	assert.Len(t, keys, 1, "промах лимитируется по корзине токена")
}
