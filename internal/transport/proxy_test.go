package transport_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-kit/ginmw"

	"github.com/cbl/cbl-api-gateway/internal/transport"
)

// TestUpstreamProxyForwards проверяет сохранение метода, пути, тела и Host
// апстрима и проброс идентификатора запроса.
func TestUpstreamProxyForwards(t *testing.T) {
	type capture struct {
		Method    string
		Path      string
		Host      string
		RequestID string
		Body      string
	}
	var seen capture
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = capture{
			Method:    r.Method,
			Path:      r.URL.Path,
			Host:      r.Host,
			RequestID: r.Header.Get(ginmw.HeaderRequestID),
			Body:      readBody(t, r),
		}
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	target, err := url.Parse(backend.URL)
	require.NoError(t, err)
	proxy := transport.NewUpstreamProxy(target, 5*time.Second, nil)
	front := httptest.NewServer(proxy)
	defer front.Close()

	request, err := http.NewRequest(http.MethodPost, front.URL+"/v1/invoices?x=1", strings.NewReader("payload"))
	require.NoError(t, err)
	request.Header.Set(ginmw.HeaderRequestID, "req-42")
	response, err := front.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()

	assert.Equal(t, http.StatusCreated, response.StatusCode)
	assert.Equal(t, "yes", response.Header.Get("X-Upstream"))
	assert.Equal(t, http.MethodPost, seen.Method)
	assert.Equal(t, "/v1/invoices", seen.Path)
	assert.Equal(t, target.Host, seen.Host, "апстрим видит свой Host")
	assert.Equal(t, "req-42", seen.RequestID)
	assert.Equal(t, "payload", seen.Body)
}

// TestUpstreamProxyFailure проверяет маскировку сбоя апстрима в 503 контракта.
func TestUpstreamProxyFailure(t *testing.T) {
	target, err := url.Parse("http://127.0.0.1:1")
	require.NoError(t, err)
	proxy := transport.NewUpstreamProxy(target, time.Second, nil)
	front := httptest.NewServer(proxy)
	defer front.Close()

	request, err := http.NewRequest(http.MethodGet, front.URL+"/v1/invoices", nil)
	require.NoError(t, err)
	request.Header.Set(ginmw.HeaderRequestID, "req-503")
	response, err := front.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()

	assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	assert.Equal(t, "1", response.Header.Get("Retry-After"))
	assert.Equal(t, "req-503", response.Header.Get(ginmw.HeaderRequestID), "503 несет идентификатор запроса")
	assert.Equal(t, ginmw.ContentTypeProblem, response.Header.Get("Content-Type"))
	var problem ginmw.Problem
	require.NoError(t, json.NewDecoder(response.Body).Decode(&problem))
	assert.Equal(t, "service_unavailable", problem.Code)
	assert.Equal(t, "req-503", problem.RequestID)
	assert.NotContains(t, problem.Detail, "127.0.0.1", "детали сбоя наружу не отдаются")
}

// TestUpstreamProxyStripsForwardingHeaders проверяет, что входящие
// forwarding-заголовки клиента удаляются: апстрим видит только канонические
// значения периметра, спуфинг клиентского IP невозможен.
func TestUpstreamProxyStripsForwardingHeaders(t *testing.T) {
	var forwardedFor, forwardedHost, realIP, forwarded string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwardedFor = r.Header.Get("X-Forwarded-For")
		forwardedHost = r.Header.Get("X-Forwarded-Host")
		realIP = r.Header.Get("X-Real-IP")
		forwarded = r.Header.Get("Forwarded")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	target, err := url.Parse(backend.URL)
	require.NoError(t, err)
	proxy := transport.NewUpstreamProxy(target, 5*time.Second, nil)
	front := httptest.NewServer(proxy)
	defer front.Close()

	request, err := http.NewRequest(http.MethodGet, front.URL+"/v1/invoices", nil)
	require.NoError(t, err)
	request.Header.Set("X-Forwarded-For", "203.0.113.7")
	request.Header.Set("X-Forwarded-Host", "evil.example.com")
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Real-IP", "203.0.113.7")
	request.Header.Set("Forwarded", "for=203.0.113.7;host=evil.example.com")
	response, err := front.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()

	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.NotContains(t, forwardedFor, "203.0.113.7", "клиентский XFF удален")
	assert.Equal(t, "127.0.0.1", forwardedFor, "XFF содержит только прямой адрес периметра")
	assert.NotEqual(t, "evil.example.com", forwardedHost, "подмененный host удален")
	assert.Empty(t, realIP, "X-Real-IP удален")
	assert.Empty(t, forwarded, "Forwarded удален")
}

// TestUpstreamProxyExpectContinue проверяет POST с Expect: 100-continue:
// тело доходит до апстрима, статус не искажается.
func TestUpstreamProxyExpectContinue(t *testing.T) {
	var body string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = readBody(t, r)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	target, err := url.Parse(backend.URL)
	require.NoError(t, err)
	proxy := transport.NewUpstreamProxy(target, 5*time.Second, nil)
	front := httptest.NewServer(proxy)
	defer front.Close()

	request, err := http.NewRequest(http.MethodPost, front.URL+"/v1/invoices", strings.NewReader("payload"))
	require.NoError(t, err)
	request.Header.Set("Expect", "100-continue")
	response, err := front.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, "payload", body)
}

// readBody вычитывает тело запроса в тестовом обработчике.
func readBody(t *testing.T, request *http.Request) string {
	t.Helper()
	defer func() { _ = request.Body.Close() }()
	data, err := io.ReadAll(request.Body)
	require.NoError(t, err)
	return string(data)
}
