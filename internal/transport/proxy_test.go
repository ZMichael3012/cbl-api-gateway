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

	"github.com/ZMichael3012/cbl-kit/ginmw"

	"github.com/cbl/cbl-api-gateway/internal/transport"
)

// TestUpstreamProxyForwards verifies that the method, path, body and upstream
// Host are preserved and the request id is forwarded.
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
	assert.Equal(t, target.Host, seen.Host, "the upstream sees its own Host")
	assert.Equal(t, "req-42", seen.RequestID)
	assert.Equal(t, "payload", seen.Body)
}

// TestUpstreamProxyFailure verifies masking of an upstream failure as the contract 503.
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
	assert.Equal(t, "req-503", response.Header.Get(ginmw.HeaderRequestID), "the 503 carries the request id")
	assert.Equal(t, ginmw.ContentTypeProblem, response.Header.Get("Content-Type"))
	var problem ginmw.Problem
	require.NoError(t, json.NewDecoder(response.Body).Decode(&problem))
	assert.Equal(t, "service_unavailable", problem.Code)
	assert.Equal(t, "req-503", problem.RequestID)
	assert.NotContains(t, problem.Detail, "127.0.0.1", "failure details are not exposed")
}

// TestUpstreamProxyStripsForwardingHeaders verifies incoming client forwarding
// headers are removed: the upstream sees only canonical perimeter values,
// client IP spoofing is impossible.
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
	assert.NotContains(t, forwardedFor, "203.0.113.7", "client XFF is removed")
	assert.Equal(t, "127.0.0.1", forwardedFor, "XFF contains only the direct perimeter address")
	assert.NotEqual(t, "evil.example.com", forwardedHost, "the spoofed host is removed")
	assert.Empty(t, realIP, "X-Real-IP is removed")
	assert.Empty(t, forwarded, "Forwarded is removed")
}

// TestUpstreamProxyExpectContinue verifies POST with Expect: 100-continue:
// the body reaches the upstream, the status is not distorted.
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

// readBody reads the request body in a test handler.
func readBody(t *testing.T, request *http.Request) string {
	t.Helper()
	defer func() { _ = request.Body.Close() }()
	data, err := io.ReadAll(request.Body)
	require.NoError(t, err)
	return string(data)
}
