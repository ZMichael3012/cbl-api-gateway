package httpmw

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/cbl/cbl-kit/ginmw"
)

// contextKeyIdentity sets the gin context key for the limit identity.
const contextKeyIdentity = "api-gateway.identity"

// IdentityFrom returns the request limit identity (empty if KeyPeek was not
// applied).
func IdentityFrom(c *gin.Context) string {
	identity, _ := c.Get(contextKeyIdentity)
	value, _ := identity.(string)
	return value
}

// SetIdentity puts the limit identity into the request context.
func SetIdentity(c *gin.Context, identity string) {
	c.Set(contextKeyIdentity, identity)
}

// AbortUnauthorized replies 401 in problem+json format without reason details;
// the response shape matches Billing API 7.2 (merchant API scope).
func AbortUnauthorized(c *gin.Context) {
	c.Header("WWW-Authenticate", `Bearer realm="billing"`)
	ginmw.AbortWithProblem(c, http.StatusUnauthorized, "unauthorized",
		"Unauthorized", "API-ключ отсутствует или недействителен")
}
