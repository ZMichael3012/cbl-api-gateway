package httpmw

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ZMichael3012/cbl-kit/ginmw"

	"github.com/cbl/cbl-api-gateway/internal/domain"
	"github.com/cbl/cbl-api-gateway/internal/usecase"
)

// RateLimit returns the merchant request limit middleware: a window counter
// (Redis, local on failure), over the limit 429 rate_limited with Retry-After
// in contract 7.1 format; the upstream is not called.
func RateLimit(perimeter *usecase.Perimeter) gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := IdentityFrom(c)
		if identity == "" {
			// The limit without authentication must not silently pass traffic:
			// an empty identity means a middleware chain assembly error.
			AbortUnauthorized(c)
			return
		}
		route := domain.RouteGroup(c.Request.URL.Path)
		decision := perimeter.Allow(c.Request.Context(), identity, route, time.Now())
		if !decision.Allowed {
			seconds := int((decision.RetryAfter + time.Second - 1) / time.Second)
			c.Header("Retry-After", strconv.Itoa(seconds))
			ginmw.AbortWithProblem(c, http.StatusTooManyRequests, "rate_limited",
				"Too Many Requests", "превышен лимит запросов мерчанта")
			return
		}
		c.Next()
	}
}
