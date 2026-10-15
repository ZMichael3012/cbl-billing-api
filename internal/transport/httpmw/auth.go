// Пакет httpmw содержит HTTP-middleware сервиса billing-api: аутентификацию
// API-ключом мерчанта (7.2).
package httpmw

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/cbl/cbl-kit/ginmw"

	"github.com/cbl/cbl-billing-api/internal/domain"
)

// contextKey задает ключ контекста gin для мерчанта запроса.
const contextKey = "billing-api.merchant-id"

// Verifier проверяет ключ и возвращает владельца; ошибка означает отказ.
type Verifier func(ctx context.Context, token string) (string, error)

// Require возвращает middleware аутентификации: строгий Bearer-заголовок
// (ровно один, схема регистронезависима по RFC 7235), проверка ключа,
// 401 problem+json при отказе; при успехе мерчант кладется в контекст для
// обработчиков /v1.
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
				// Проверка недоступна (БД): запрос не аутентифицирован, но это
				// отказ зависимости, а не ключа - 503 с повтором.
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

// MerchantID возвращает мерчанта аутентифицированного запроса (пусто, если
// middleware не применялся).
func MerchantID(c *gin.Context) string {
	merchantID, _ := c.Get(contextKey)
	value, _ := merchantID.(string)
	return value
}

// AbortUnauthorized отвечает 401 в формате problem+json без деталей причины;
// заголовок WWW-Authenticate соответствует схеме bearerAuth контракта.
func AbortUnauthorized(c *gin.Context) {
	c.Header("WWW-Authenticate", `Bearer realm="billing"`)
	ginmw.AbortWithProblem(c, http.StatusUnauthorized, "unauthorized",
		"Unauthorized", "API-ключ отсутствует или недействителен")
}

// bearerToken извлекает токен из Authorization: схема Bearer
// регистронезависима, токен - без пробелов внутри.
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
