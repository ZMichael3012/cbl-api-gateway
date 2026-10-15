package httpmw

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/cbl/cbl-kit/ginmw"
)

// contextKeyIdentity задает ключ контекста gin для идентификатора лимита.
const contextKeyIdentity = "api-gateway.identity"

// IdentityFrom возвращает идентификатор лимита запроса (пусто, если KeyPeek
// не применялся).
func IdentityFrom(c *gin.Context) string {
	identity, _ := c.Get(contextKeyIdentity)
	value, _ := identity.(string)
	return value
}

// SetIdentity кладет идентификатор лимита в контекст запроса.
func SetIdentity(c *gin.Context, identity string) {
	c.Set(contextKeyIdentity, identity)
}

// AbortUnauthorized отвечает 401 в формате problem+json без деталей причины;
// форма ответа совпадает с Billing API 7.2 (скоуп мерчантского API).
func AbortUnauthorized(c *gin.Context) {
	c.Header("WWW-Authenticate", `Bearer realm="billing"`)
	ginmw.AbortWithProblem(c, http.StatusUnauthorized, "unauthorized",
		"Unauthorized", "API-ключ отсутствует или недействителен")
}
