package httpmw

import (
	"github.com/gin-gonic/gin"

	"github.com/cbl/cbl-api-gateway/internal/domain"
	"github.com/cbl/cbl-api-gateway/internal/usecase"
)

// KeyPeek returns the API key control middleware for the perimeter (contract
// 7.2 FR-6): strict Bearer parsing, apikey:v1 read, deny rejects the request
// without calling the Billing API; a miss is delegated to the BFF (the source
// of truth), and the limit identity (merchant or token:<hex>) is put into the
// context.
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
			// Revoked key tombstone: reject before the upstream, same as BFF 7.2.
			AbortUnauthorized(c)
			return
		}
		SetIdentity(c, domain.IdentityKey(check.MerchantID, tokenHash))
		c.Next()
	}
}
