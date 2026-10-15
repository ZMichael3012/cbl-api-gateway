package httpmw

import (
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/cbl/cbl-kit/ginmw"
)

// requestIDPattern задает допустимую форму клиентского идентификатора запроса.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

// SanitizeRequestID удаляет небезопасный клиентский X-Request-Id до
// ginmw.RequestID: произвольная строка клиента иначе попадает в логи, ответ и
// problem.request_id всех сервисов (подмена корреляции, засорение логов);
// небезопасный идентификатор заменяется сгенерированным периметром.
func SanitizeRequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := strings.TrimSpace(c.GetHeader(ginmw.HeaderRequestID))
		if id != "" && !requestIDPattern.MatchString(id) {
			c.Request.Header.Del(ginmw.HeaderRequestID)
		}
		c.Next()
	}
}
