package httpmw

import (
	"github.com/gin-gonic/gin"

	"github.com/cbl/cbl-api-gateway/internal/domain"
	"github.com/cbl/cbl-api-gateway/internal/usecase"
)

// KeyPeek возвращает middleware контроля API-ключа на периметре (контракт
// 7.2 FR-6): строгий Bearer, чтение apikey:v1, deny запрещает запрос без
// похода в Billing API; промах делегируется BFF (источник истины), а
// идентификатор лимита (мерчант или token:<hex>) кладется в контекст.
func KeyPeek(perimeter *usecase.Perimeter) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := domain.AuthorizationToken(c.Request.Header.Values("Authorization"))
		if !ok {
			AbortUnauthorized(c)
			return
		}
		tokenHash := domain.TokenHash(token)
		check := perimeter.CheckKey(c.Request.Context(), tokenHash)
		if check.Deny {
			// Tombstone отозванного ключа: отказ до апстрима, как в BFF 7.2.
			AbortUnauthorized(c)
			return
		}
		SetIdentity(c, domain.IdentityKey(check.MerchantID, tokenHash))
		c.Next()
	}
}
