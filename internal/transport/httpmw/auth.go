// Package httpmw contains the HTTP middleware of the billing-api service:
// merchant API key authentication (7.2).
package httpmw

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ZMichael3012/cbl-kit/ginmw"

	"github.com/cbl/cbl-billing-api/internal/domain"
)

// contextKey sets the gin context key for the merchant of the request.
const contextKey = "billing-api.merchant-id"

// Verifier checks the key and returns its owner; an error means rejection.
type Verifier func(ctx context.Context, token string) (string, error)

// Require returns the authentication middleware: a strict Bearer header
// (exactly one, the scheme is case-insensitive per RFC 7235), key verification,
// 401 problem+json on rejection; on success the merchant is put into the
// context for /v1 handlers.
func Require(verify Verifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		if len(c.Request.Header.Values("Authorization")) != 1 {
			AbortUnauthorized(c)
			return
		}
		token, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			AbortUnauthorized(c)
			return
		}
		merchantID, err := verify(c.Request.Context(), token)
		if err != nil {
			if !errors.Is(err, domain.ErrUnauthorized) {
				// Verification is unavailable (DB): the request is not
				// authenticated, but this is a dependency failure, not a key
				// failure: 503 with a retry.
				c.Header("Retry-After", "1")
				ginmw.AbortWithProblem(c, http.StatusServiceUnavailable, "service_unavailable",
					"Service Unavailable", "проверка ключа временно недоступна")
				return
			}
			AbortUnauthorized(c)
			return
		}
		c.Set(contextKey, merchantID)
		c.Next()
	}
}

// MerchantID returns the merchant of the authenticated request (empty if the
// middleware was not applied).
func MerchantID(c *gin.Context) string {
	merchantID, _ := c.Get(contextKey)
	value, _ := merchantID.(string)
	return value
}

// AbortUnauthorized responds 401 in problem+json format without reason details;
// the WWW-Authenticate header matches the contract bearerAuth scheme.
func AbortUnauthorized(c *gin.Context) {
	c.Header("WWW-Authenticate", `Bearer realm="billing"`)
	ginmw.AbortWithProblem(c, http.StatusUnauthorized, "unauthorized",
		"Unauthorized", "API-ключ отсутствует или недействителен")
}

// bearerToken extracts the token from Authorization: the Bearer scheme is
// case-insensitive, the token has no spaces inside.
func bearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(strings.TrimSpace(header), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}
