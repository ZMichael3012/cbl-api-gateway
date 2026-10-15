package transport_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-kit/ginmw"

	"github.com/cbl/cbl-api-gateway/internal/transport"
	"github.com/cbl/cbl-api-gateway/internal/usecase"
)

// fakePeeker управляет значением кэша проверки ключей.
type fakePeeker struct {
	value   string
	present bool
}

// Get возвращает заданное значение.
func (f *fakePeeker) Get(_ context.Context, _ string) (string, bool, error) {
	return f.value, f.present, nil
}

// windowCounter считает по ключам окна, как Redis.
type windowCounter struct {
	counts map[string]int64
}

// Incr увеличивает счетчик ключа.
func (c *windowCounter) Incr(_ context.Context, key string, _ time.Duration) (int64, error) {
	c.counts[key]++
	return c.counts[key], nil
}

// newBackend поднимает stub-апстрим, отражающий путь и идентификатор запроса.
func newBackend(t *testing.T) *httptest.Server {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"path":       r.URL.Path,
			"request_id": r.Header.Get(ginmw.HeaderRequestID),
		})
	}))
	t.Cleanup(backend.Close)
	return backend
}

// newPerimeter собирает сервис периметра на локальном счетчике.
func newPerimeter(t *testing.T, peeker *fakePeeker, limit int64) *usecase.Perimeter {
	t.Helper()
	perimeter, err := usecase.NewPerimeter(peeker, &windowCounter{counts: map[string]int64{}}, nil, limit, time.Second)
	require.NoError(t, err)
	return perimeter
}

// newRouter собирает периметр с одним stub-апстримом.
func newRouter(t *testing.T, peeker *fakePeeker, limit int64) (*gin.Engine, *httptest.Server) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	backend := newBackend(t)
	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)
	router, err := transport.NewRouter(transport.Config{
		Perimeter:       newPerimeter(t, peeker, limit),
		BillingAPI:      backendURL,
		Backoffice:      backendURL,
		UpstreamTimeout: 5 * time.Second,
	})
	require.NoError(t, err)
	return router, backend
}

// doRouter выполняет запрос через маршрутизатор периметра.
func doRouter(router *gin.Engine, method, path, authorization string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// decodeProblem разбирает problem+json ответа.
func decodeProblem(t *testing.T, recorder *httptest.ResponseRecorder) ginmw.Problem {
	t.Helper()
	var problem ginmw.Problem
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &problem))
	return problem
}

// TestRouterValidation проверяет fail-fast сборки маршрутов.
func TestRouterValidation(t *testing.T) {
	_, err := transport.NewRouter(transport.Config{})
	require.Error(t, err)
	perimeter, err := usecase.NewPerimeter(nil, &windowCounter{counts: map[string]int64{}}, nil, 1, time.Second)
	require.NoError(t, err)
	upstream, err := url.Parse("http://localhost:9999")
	require.NoError(t, err)
	_, err = transport.NewRouter(transport.Config{Perimeter: perimeter, BillingAPI: upstream, Backoffice: upstream})
	require.Error(t, err, "нулевой бюджет времени запрещен")
}

// TestRouterUpstreamMapping проверяет, что /v1 уходит только в Billing API, а
// /admin/v1 - только в Back-office.
func TestRouterUpstreamMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"upstream":"billing"}`))
	}))
	t.Cleanup(billing.Close)
	backoffice := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"upstream":"backoffice"}`))
	}))
	t.Cleanup(backoffice.Close)
	billingURL, err := url.Parse(billing.URL)
	require.NoError(t, err)
	backofficeURL, err := url.Parse(backoffice.URL)
	require.NoError(t, err)
	router, err := transport.NewRouter(transport.Config{
		Perimeter:       newPerimeter(t, &fakePeeker{}, 10),
		BillingAPI:      billingURL,
		Backoffice:      backofficeURL,
		UpstreamTimeout: 5 * time.Second,
	})
	require.NoError(t, err)

	merchantRoute := doRouter(router, http.MethodGet, "/v1/balances", "Bearer cbl_v1_token")
	require.Equal(t, http.StatusOK, merchantRoute.Code)
	assert.Contains(t, merchantRoute.Body.String(), "billing")
	assert.NotContains(t, merchantRoute.Body.String(), "backoffice")

	adminRoute := doRouter(router, http.MethodGet, "/admin/v1/payouts", "")
	require.Equal(t, http.StatusOK, adminRoute.Code)
	assert.Contains(t, adminRoute.Body.String(), "backoffice")
	assert.NotContains(t, adminRoute.Body.String(), "billing")
}

// TestRouterNotFound проверяет 404 problem+json для неизвестного пути.
func TestRouterNotFound(t *testing.T) {
	router, _ := newRouter(t, &fakePeeker{}, 10)
	recorder := doRouter(router, http.MethodGet, "/unknown", "")
	require.Equal(t, http.StatusNotFound, recorder.Code)
	assert.Equal(t, ginmw.ContentTypeProblem, recorder.Header().Get("Content-Type"))
	assert.Equal(t, "not_found", decodeProblem(t, recorder).Code)
}

// TestRouterV1Allowed проверяет проксирование подтвержденного ключа и проброс
// идентификатора запроса до апстрима.
func TestRouterV1Allowed(t *testing.T) {
	merchantID := "01912f2f-6f82-7d5e-8e04-4d4b5b6a1234"
	router, _ := newRouter(t, &fakePeeker{value: "ok:" + merchantID, present: true}, 10)
	request := httptest.NewRequest(http.MethodGet, "/v1/invoices/42", nil)
	request.Header.Set("Authorization", "Bearer cbl_v1_token")
	request.Header.Set(ginmw.HeaderRequestID, "req-it-0001")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"path":"/v1/invoices/42"`)
	assert.Contains(t, recorder.Body.String(), `"request_id":"req-it-0001"`, "сквозной идентификатор доходит до апстрима")
	assert.Equal(t, "req-it-0001", recorder.Header().Get(ginmw.HeaderRequestID))
	assert.Len(t, recorder.Header().Values(ginmw.HeaderRequestID), 1, "заголовок ответа не дублируется")
}

// TestRouterV1Denied проверяет отказ tombstone без вызова апстрима.
func TestRouterV1Denied(t *testing.T) {
	router, _ := newRouter(t, &fakePeeker{value: "deny", present: true}, 10)
	recorder := doRouter(router, http.MethodGet, "/v1/invoices/42", "Bearer cbl_v1_token")
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	assert.Equal(t, `Bearer realm="billing"`, recorder.Header().Get("WWW-Authenticate"))
}

// TestRouterV1RateLimited проверяет 429 на маршруте после превышения лимита;
// запросы шлются до отказа, чтобы тест не зависел от границы секунды.
func TestRouterV1RateLimited(t *testing.T) {
	router, _ := newRouter(t, &fakePeeker{}, 1)
	var denied *httptest.ResponseRecorder
	for attempt := 0; attempt < 8; attempt++ {
		recorder := doRouter(router, http.MethodGet, "/v1/balances", "Bearer cbl_v1_token")
		if recorder.Code == http.StatusTooManyRequests {
			denied = recorder
			break
		}
		require.Equal(t, http.StatusOK, recorder.Code)
	}
	require.NotNil(t, denied, "лимит обязан сработать в пределах нескольких попыток")
	assert.Equal(t, "1", denied.Header().Get("Retry-After"))
	assert.Equal(t, ginmw.ContentTypeProblem, denied.Header().Get("Content-Type"))
	assert.Equal(t, "rate_limited", decodeProblem(t, denied).Code)
}

// TestRouterMissDelegated проверяет делегирование неизвестного ключа BFF.
func TestRouterMissDelegated(t *testing.T) {
	router, _ := newRouter(t, &fakePeeker{}, 10)
	recorder := doRouter(router, http.MethodGet, "/v1/invoices", "Bearer cbl_v1_token")
	assert.Equal(t, http.StatusOK, recorder.Code, "решение о ключе принимает BFF")
}

// TestRouterAdminProxy проверяет проксирование /admin/v1 без лимита мерчанта.
func TestRouterAdminProxy(t *testing.T) {
	router, _ := newRouter(t, &fakePeeker{}, 10)
	recorder := doRouter(router, http.MethodGet, "/admin/v1/payouts", "")
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"path":"/admin/v1/payouts"`)
}

// TestRouterBarePaths проверяет отключение редиректов Gin и пути без сегментов:
// POST не теряет тело на 301, /v1 и /v1/ проксируются как есть.
func TestRouterBarePaths(t *testing.T) {
	router, _ := newRouter(t, &fakePeeker{}, 10)
	for _, path := range []string{"/v1", "/v1/"} {
		recorder := doRouter(router, http.MethodGet, path, "Bearer cbl_v1_token")
		require.Equal(t, http.StatusOK, recorder.Code, path)
		assert.Contains(t, recorder.Body.String(), `"path":"`+path+`"`)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/invoices", nil)
	request.Header.Set("Authorization", "Bearer cbl_v1_token")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusOK, recorder.Code, "POST не редиректится")
}

// TestRouterStreamingFlush проверяет потоковый ответ (SSE): обертка
// proxyWriter пробрасывает Flush, тело доходит целиком.
func TestRouterStreamingFlush(t *testing.T) {
	gin.SetMode(gin.TestMode)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)
		_, _ = io.WriteString(w, "data: one\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: two\n\n")
		flusher.Flush()
	}))
	t.Cleanup(backend.Close)
	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)
	router, err := transport.NewRouter(transport.Config{
		Perimeter:       newPerimeter(t, &fakePeeker{}, 10),
		BillingAPI:      backendURL,
		Backoffice:      backendURL,
		UpstreamTimeout: 5 * time.Second,
	})
	require.NoError(t, err)

	recorder := doRouter(router, http.MethodGet, "/v1/stream", "Bearer cbl_v1_token")
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "data: one")
	assert.Contains(t, recorder.Body.String(), "data: two")
	assert.True(t, recorder.Flushed, "потоковые данные отправляются без буферизации")
}

// TestRouterRequestIDFallback проверяет, что ответ без заголовка апстрима
// получает идентификатор периметра один раз.
func TestRouterRequestIDFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)
	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)
	router, err := transport.NewRouter(transport.Config{
		Perimeter:       newPerimeter(t, &fakePeeker{}, 10),
		BillingAPI:      backendURL,
		Backoffice:      backendURL,
		UpstreamTimeout: 5 * time.Second,
	})
	require.NoError(t, err)
	recorder := doRouter(router, http.MethodGet, "/v1/ping", "Bearer cbl_v1_token")
	require.Equal(t, http.StatusNoContent, recorder.Code)
	assert.Len(t, recorder.Header().Values(ginmw.HeaderRequestID), 1)
	assert.NotEmpty(t, recorder.Header().Get(ginmw.HeaderRequestID))
}
