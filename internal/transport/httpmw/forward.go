package httpmw

import (
	"github.com/gin-gonic/gin"

	"github.com/cbl/cbl-kit/ginmw"
)

// ForwardRequestID переносит идентификатор запроса периметра в заголовок
// запроса к апстриму: сквозная корреляция логов и problem.request_id (7.1).
func ForwardRequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Header.Set(ginmw.HeaderRequestID, ginmw.RequestIDFrom(c))
		c.Next()
	}
}
