package httpmw

import (
	"github.com/gin-gonic/gin"

	"github.com/ZMichael3012/cbl-kit/ginmw"
)

// ForwardRequestID moves the perimeter request id into the upstream request
// header: end-to-end log correlation and problem.request_id (7.1).
func ForwardRequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Header.Set(ginmw.HeaderRequestID, ginmw.RequestIDFrom(c))
		c.Next()
	}
}
