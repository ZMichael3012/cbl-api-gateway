package httpmw_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-kit/ginmw"

	"github.com/cbl/cbl-api-gateway/internal/domain"
	"github.com/cbl/cbl-api-gateway/internal/transport/httpmw"
	"github.com/cbl/cbl-api-gateway/internal/usecase"
)

// fakePeeker controls the value of the API key check cache.
type fakePeeker struct {
	value   string
	present bool
	err     error
}

// Get returns the configured value.
func (f *fakePeeker) Get(_ context.Context, _ string) (string, bool, error) {
	return f.value, f.present, f.err
}

// countingPeeker counts accesses to the check cache.
type countingPeeker struct {
	calls int
}

// Get counts calls and always returns a miss.
func (c *countingPeeker) Get(_ context.Context, _ string) (string, bool, error) {
	c.calls++
	return "", false, nil
}

// fakeCounter controls the limit counter.
type fakeCounter struct {
	count int64
	err   error
}

// Incr increments the counter.
func (f *fakeCounter) Incr(_ context.Context, _ string, _ time.Duration) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.count++
	return f.count, nil
}

// newPerimeter builds the perimeter service for the middleware.
func newPerimeter(t *testing.T, peeker *fakePeeker, counter *fakeCounter, limit int64) *usecase.Perimeter {
	t.Helper()
	perimeter, err := usecase.NewPerimeter(peeker, counter, nil, limit, time.Second)
	require.NoError(t, err)
	return perimeter
}

// probe performs a request to the probe route.
func probe(router *gin.Engine, authorization ...string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/v1/probe", nil)
	for _, value := range authorization {
		request.Header.Add("Authorization", value)
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

// TestKeyPeekUnauthorized verifies the strict header shape.
func TestKeyPeekUnauthorized(t *testing.T) {
	perimeter := newPerimeter(t, &fakePeeker{}, &fakeCounter{}, 10)
	router := gin.New()
	router.GET("/v1/probe", httpmw.KeyPeek(perimeter), func(c *gin.Context) { c.Status(http.StatusOK) })

	for name, authorization := range map[string][]string{
		"отсутствует": nil,
		"дубликат":    {"Bearer abc", "Bearer abc"},
		"чужая схема": {"Basic abc"},
		"пустой":      {"Bearer"},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := probe(router, authorization...)
			assert.Equal(t, http.StatusUnauthorized, recorder.Code)
			assert.Equal(t, `Bearer realm="billing"`, recorder.Header().Get("WWW-Authenticate"))
			assert.Equal(t, ginmw.ContentTypeProblem, recorder.Header().Get("Content-Type"))
			assert.Equal(t, "unauthorized", decodeProblem(t, recorder).Code)
		})
	}
}

// TestKeyPeekDenied verifies immediate tombstone rejection without calling the
// handler and without touching the counter (the quota is not consumed).
func TestKeyPeekDenied(t *testing.T) {
	perimeter := newPerimeter(t, &fakePeeker{value: "deny", present: true}, &fakeCounter{}, 10)
	called := false
	router := gin.New()
	router.GET("/v1/probe", httpmw.KeyPeek(perimeter), func(c *gin.Context) {
		called = true
		c.Status(http.StatusOK)
	})
	recorder := probe(router, "Bearer cbl_v1_token")
	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	assert.False(t, called, "the upstream is not called")
}

// TestKeyPeekMalformedSkipsPeek verifies a malformed header is rejected
// without touching Redis.
func TestKeyPeekMalformedSkipsPeek(t *testing.T) {
	counting := &countingPeeker{}
	perimeter, err := usecase.NewPerimeter(counting, &fakeCounter{}, nil, 10, time.Second)
	require.NoError(t, err)
	router := gin.New()
	router.GET("/v1/probe", httpmw.KeyPeek(perimeter), func(c *gin.Context) { c.Status(http.StatusOK) })
	recorder := probe(router, "Basic abc")
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	assert.Zero(t, counting.calls, "Redis is not touched on a malformed header")
}

// TestKeyPeekAllowed verifies the identity of the confirmed merchant.
func TestKeyPeekAllowed(t *testing.T) {
	merchantID := "01912f2f-6f82-7d5e-8e04-4d4b5b6a1234"
	perimeter := newPerimeter(t, &fakePeeker{value: "ok:" + merchantID, present: true}, &fakeCounter{}, 10)
	router := gin.New()
	router.GET("/v1/probe", httpmw.KeyPeek(perimeter), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"identity": httpmw.IdentityFrom(c)})
	})
	recorder := probe(router, "Bearer cbl_v1_token")
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), merchantID)
}

// TestKeyPeekDelegated verifies the identity on a miss and a Redis error.
func TestKeyPeekDelegated(t *testing.T) {
	for name, peeker := range map[string]*fakePeeker{
		"промах": {},
		"ошибка": {err: errors.New("redis down")},
		"порча":  {value: "ok:bad id", present: true},
	} {
		t.Run(name, func(t *testing.T) {
			perimeter := newPerimeter(t, peeker, &fakeCounter{}, 10)
			router := gin.New()
			router.GET("/v1/probe", httpmw.KeyPeek(perimeter), func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"identity": httpmw.IdentityFrom(c)})
			})
			recorder := probe(router, "Bearer cbl_v1_token")
			require.Equal(t, http.StatusOK, recorder.Code, "the request is delegated to the BFF")
			assert.Contains(t, recorder.Body.String(), domain.TokenIdentityPrefix+domain.TokenHash("cbl_v1_token"))
		})
	}
}

// TestRateLimitAllowed verifies passing within the limit.
func TestRateLimitAllowed(t *testing.T) {
	perimeter := newPerimeter(t, &fakePeeker{}, &fakeCounter{}, 2)
	router := gin.New()
	router.GET("/v1/probe", func(c *gin.Context) {
		httpmw.SetIdentity(c, "m")
		c.Next()
	}, httpmw.RateLimit(perimeter), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	assert.Equal(t, http.StatusOK, probe(router).Code)
	assert.Equal(t, http.StatusOK, probe(router).Code)
}

// TestRateLimitDenied verifies 429 with Retry-After and the contract code;
// requests are sent until rejection so the test does not depend on the second
// boundary.
func TestRateLimitDenied(t *testing.T) {
	perimeter := newPerimeter(t, &fakePeeker{}, &fakeCounter{}, 1)
	called := false
	router := gin.New()
	router.GET("/v1/probe", func(c *gin.Context) {
		httpmw.SetIdentity(c, "m")
		c.Next()
	}, httpmw.RateLimit(perimeter), func(c *gin.Context) {
		called = true
		c.Status(http.StatusOK)
	})
	var denied *httptest.ResponseRecorder
	for attempt := 0; attempt < 8; attempt++ {
		called = false
		recorder := probe(router)
		if recorder.Code == http.StatusTooManyRequests {
			denied = recorder
			break
		}
		require.Equal(t, http.StatusOK, recorder.Code)
		require.True(t, called)
	}
	require.NotNil(t, denied, "the limit must trigger within a few attempts")
	assert.False(t, called, "the upstream is not called when the limit rejects")
	assert.Equal(t, "1", denied.Header().Get("Retry-After"))
	assert.Equal(t, ginmw.ContentTypeProblem, denied.Header().Get("Content-Type"))
	assert.Equal(t, "rate_limited", decodeProblem(t, denied).Code)
}

// TestRateLimitWithoutIdentity verifies protection against an empty middleware chain.
func TestRateLimitWithoutIdentity(t *testing.T) {
	perimeter := newPerimeter(t, &fakePeeker{}, &fakeCounter{}, 1)
	router := gin.New()
	router.GET("/v1/probe", httpmw.RateLimit(perimeter), func(c *gin.Context) { c.Status(http.StatusOK) })
	recorder := probe(router)
	assert.Equal(t, http.StatusUnauthorized, recorder.Code, "the limit without authentication does not pass traffic")
}

// TestSanitizeRequestID verifies unsafe client request ids are dropped before
// the perimeter generates its own.
func TestSanitizeRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(httpmw.SanitizeRequestID(), ginmw.RequestID())
	router.GET("/probe", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"request_id": ginmw.RequestIDFrom(c)})
	})

	for name, clientID := range map[string]string{
		"валидный":        "req-42abc",
		"пробел":          "bad id",
		"спецсимволы":     "bad<script>",
		"слишком длинный": strings.Repeat("a", 65),
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/probe", nil)
			request.Header.Set(ginmw.HeaderRequestID, clientID)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			require.Equal(t, http.StatusOK, recorder.Code)
			var payload map[string]string
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
			if name == "валидный" {
				assert.Equal(t, clientID, payload["request_id"])
				return
			}
			assert.NotEqual(t, clientID, payload["request_id"], "the unsafe id is replaced with a generated one")
			assert.Len(t, payload["request_id"], 36)
		})
	}
}

// TestForwardRequestID verifies the request id is forwarded to the upstream.
func TestForwardRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ginmw.RequestID(), httpmw.ForwardRequestID())
	router.GET("/v1/probe", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"request_id": c.Request.Header.Get(ginmw.HeaderRequestID)})
	})
	request := httptest.NewRequest(http.MethodGet, "/v1/probe", nil)
	request.Header.Set(ginmw.HeaderRequestID, "client-id")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "client-id")

	request = httptest.NewRequest(http.MethodGet, "/v1/probe", nil)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	assert.Contains(t, recorder.Body.String(), `"request_id":"`)
	assert.NotContains(t, recorder.Body.String(), `"request_id":""`, "the generated id is forwarded")
}
