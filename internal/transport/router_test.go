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

	"github.com/ZMichael3012/cbl-kit/ginmw"

	"github.com/cbl/cbl-api-gateway/internal/transport"
	"github.com/cbl/cbl-api-gateway/internal/usecase"
)

// fakePeeker controls the value of the API key check cache.
type fakePeeker struct {
	value   string
	present bool
}

// Get returns the configured value.
func (f *fakePeeker) Get(_ context.Context, _ string) (string, bool, error) {
	return f.value, f.present, nil
}

// windowCounter counts by window keys like Redis.
type windowCounter struct {
	counts map[string]int64
}

// Incr increments the key counter.
func (c *windowCounter) Incr(_ context.Context, key string, _ time.Duration) (int64, error) {
	c.counts[key]++
	return c.counts[key], nil
}

// newBackend starts a stub upstream that echoes the path and the request id.
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

// newPerimeter builds the perimeter service on a local counter.
func newPerimeter(t *testing.T, peeker *fakePeeker, limit int64) *usecase.Perimeter {
	t.Helper()
	perimeter, err := usecase.NewPerimeter(peeker, &windowCounter{counts: map[string]int64{}}, nil, limit, time.Second)
	require.NoError(t, err)
	return perimeter
}

// newRouter builds the perimeter with a single stub upstream.
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

// doRouter performs a request through the perimeter router.
func doRouter(router *gin.Engine, method, path, authorization string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// decodeProblem decodes the problem+json response body.
func decodeProblem(t *testing.T, recorder *httptest.ResponseRecorder) ginmw.Problem {
	t.Helper()
	var problem ginmw.Problem
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &problem))
	return problem
}

// TestRouterValidation verifies route construction fail-fast.
func TestRouterValidation(t *testing.T) {
	_, err := transport.NewRouter(transport.Config{})
	require.Error(t, err)
	perimeter, err := usecase.NewPerimeter(nil, &windowCounter{counts: map[string]int64{}}, nil, 1, time.Second)
	require.NoError(t, err)
	upstream, err := url.Parse("http://localhost:9999")
	require.NoError(t, err)
	_, err = transport.NewRouter(transport.Config{Perimeter: perimeter, BillingAPI: upstream, Backoffice: upstream})
	require.Error(t, err, "zero time budget is forbidden")
}

// TestRouterUpstreamMapping verifies that /v1 goes only to the Billing API and
// /admin/v1 goes only to the Back-office.
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

// TestRouterNotFound verifies the 404 problem+json for an unknown path.
func TestRouterNotFound(t *testing.T) {
	router, _ := newRouter(t, &fakePeeker{}, 10)
	recorder := doRouter(router, http.MethodGet, "/unknown", "")
	require.Equal(t, http.StatusNotFound, recorder.Code)
	assert.Equal(t, ginmw.ContentTypeProblem, recorder.Header().Get("Content-Type"))
	assert.Equal(t, "not_found", decodeProblem(t, recorder).Code)
}

// TestRouterV1Allowed verifies proxying of a confirmed key and forwarding of
// the request id to the upstream.
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
	assert.Contains(t, recorder.Body.String(), `"request_id":"req-it-0001"`, "the end-to-end id reaches the upstream")
	assert.Equal(t, "req-it-0001", recorder.Header().Get(ginmw.HeaderRequestID))
	assert.Len(t, recorder.Header().Values(ginmw.HeaderRequestID), 1, "the response header is not duplicated")
}

// TestRouterV1Denied verifies tombstone rejection without calling the upstream.
func TestRouterV1Denied(t *testing.T) {
	router, _ := newRouter(t, &fakePeeker{value: "deny", present: true}, 10)
	recorder := doRouter(router, http.MethodGet, "/v1/invoices/42", "Bearer cbl_v1_token")
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	assert.Equal(t, `Bearer realm="billing"`, recorder.Header().Get("WWW-Authenticate"))
}

// TestRouterV1RateLimited verifies 429 on the route after the limit is
// exceeded; requests are sent until rejection so the test does not depend on
// the second boundary.
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
	require.NotNil(t, denied, "the limit must trigger within a few attempts")
	assert.Equal(t, "1", denied.Header().Get("Retry-After"))
	assert.Equal(t, ginmw.ContentTypeProblem, denied.Header().Get("Content-Type"))
	assert.Equal(t, "rate_limited", decodeProblem(t, denied).Code)
}

// TestRouterMissDelegated verifies an unknown key is delegated to the BFF.
func TestRouterMissDelegated(t *testing.T) {
	router, _ := newRouter(t, &fakePeeker{}, 10)
	recorder := doRouter(router, http.MethodGet, "/v1/invoices", "Bearer cbl_v1_token")
	assert.Equal(t, http.StatusOK, recorder.Code, "the BFF makes the key decision")
}

// TestRouterAdminProxy verifies /admin/v1 proxying without a merchant limit.
func TestRouterAdminProxy(t *testing.T) {
	router, _ := newRouter(t, &fakePeeker{}, 10)
	recorder := doRouter(router, http.MethodGet, "/admin/v1/payouts", "")
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"path":"/admin/v1/payouts"`)
}

// TestRouterBarePaths verifies Gin redirects are disabled and paths without
// segments work: POST does not lose its body to a 301, /v1 and /v1/ are
// proxied as is.
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
	assert.Equal(t, http.StatusOK, recorder.Code, "POST is not redirected")
}

// TestRouterStreamingFlush verifies a streaming response (SSE): the
// proxyWriter wrapper forwards Flush, the body arrives in full.
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
	assert.True(t, recorder.Flushed, "streaming data is sent without buffering")
}

// TestRouterRequestIDFallback verifies a response without the upstream header
// receives the perimeter id exactly once.
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
