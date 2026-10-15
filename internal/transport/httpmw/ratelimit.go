package httpmw

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cbl/cbl-kit/ginmw"

	"github.com/cbl/cbl-api-gateway/internal/domain"
	"github.com/cbl/cbl-api-gateway/internal/usecase"
)

// RateLimit возвращает middleware лимита запросов мерчанта: счетчик окна
// (Redis, при отказе - локальный), при превышении 429 rate_limited с
// Retry-After в формате контракта 7.1; апстрим не вызывается.
func RateLimit(perimeter *usecase.Perimeter) gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := IdentityFrom(c)
		if identity == "" {
			// Лимит без аутентификации не должен молча пропускать трафик:
			// пустой идентификатор означает ошибку сборки цепочки middleware.
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
