package transport

import (
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/cbl/cbl-kit/ginmw"

	"github.com/cbl/cbl-api-gateway/internal/transport/httpmw"
	"github.com/cbl/cbl-api-gateway/internal/usecase"
)

// Config describes the dependencies for building the perimeter routes.
type Config struct {
	// Perimeter applies API key control and limits on /v1 routes.
	Perimeter *usecase.Perimeter
	// BillingAPI sets the merchant API upstream.
	BillingAPI *url.URL
	// Backoffice sets the operator API upstream.
	Backoffice *url.URL
	// UpstreamTimeout sets the upstream response wait budget.
	UpstreamTimeout time.Duration
	// Log sets the service logger.
	Log *zap.Logger
}

// NewRouter builds the perimeter HTTP routing: /v1 with API key control and a
// limit, /admin/v1 without a merchant limit (RBAC is 12.x), everything else -
// 404 problem+json; upstream failures are returned as 503 (contract 7.1).
func NewRouter(cfg Config) (*gin.Engine, error) {
	if cfg.Perimeter == nil {
		return nil, errors.New("perimeter is required")
	}
	if cfg.BillingAPI == nil || cfg.Backoffice == nil {
		return nil, errors.New("upstream urls are required")
	}
	if cfg.UpstreamTimeout <= 0 {
		return nil, errors.New("upstream timeout must be positive")
	}
	if cfg.Log == nil {
		cfg.Log = zap.NewNop()
	}

	router := gin.New()
	// Redirects are disabled: a 301 on POST drops the body and breaks the
	// contract; path normalization belongs to the upstream contract, not the
	// perimeter.
	router.RedirectTrailingSlash = false
	router.RedirectFixedPath = false
	router.Use(
		httpmw.SanitizeRequestID(),
		ginmw.RequestID(),
		ginmw.AccessLog(cfg.Log),
		ginmw.Recovery(cfg.Log),
		httpmw.ForwardRequestID(),
	)
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.GET("/readyz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	billingProxy := NewUpstreamProxy(cfg.BillingAPI, cfg.UpstreamTimeout, cfg.Log)
	backofficeProxy := NewUpstreamProxy(cfg.Backoffice, cfg.UpstreamTimeout, cfg.Log)

	v1 := router.Group("/v1", httpmw.KeyPeek(cfg.Perimeter), httpmw.RateLimit(cfg.Perimeter))
	v1.Any("", proxyHandler(billingProxy))
	v1.Any("/*path", proxyHandler(billingProxy))

	admin := router.Group("/admin/v1")
	admin.Any("", proxyHandler(backofficeProxy))
	admin.Any("/*path", proxyHandler(backofficeProxy))

	router.NoRoute(func(c *gin.Context) {
		ginmw.AbortWithProblem(c, http.StatusNotFound, "not_found", "Not Found", "")
	})
	return router, nil
}

// proxyHandler adapts the reverse proxy to a Gin handler: the wrapper hides
// gin's optional interfaces (its CloseNotify panics on an incompatible Writer,
// for example httptest.Recorder); request cancellation is tracked through the
// request context, which the server cancels when the client connection drops.
// The X-Request-Id header preset by ginmw is removed: the upstream response
// header is added via Header.Add and without removal it would be duplicated.
func proxyHandler(proxy *httputil.ReverseProxy) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Del(ginmw.HeaderRequestID)
		proxy.ServeHTTP(proxyWriter{ResponseWriter: c.Writer}, c.Request)
	}
}

// proxyWriter hides gin's CloseNotify from ReverseProxy and forwards Flush for
// streaming responses.
type proxyWriter struct {
	http.ResponseWriter
}

// Flush sends the accumulated buffer if the underlying Writer supports it.
func (w proxyWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
