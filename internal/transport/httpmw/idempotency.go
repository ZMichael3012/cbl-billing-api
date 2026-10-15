package httpmw

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cbl/cbl-kit/ginmw"

	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/usecase"
)

// Константы middleware идемпотентности.
const (
	// idempotencyHeader задает обязательный заголовок ключа идемпотентности.
	idempotencyHeader = "Idempotency-Key"
	// MaxIdempotencyRequestBytes задает предел тела мутирующего запроса:
	// защита от memory DoS (снимок ответа ограничен отдельно, 256 KiB).
	MaxIdempotencyRequestBytes = 1 << 20
	// finishTimeout задает бюджет фиксации ответа после отмены запроса клиентом.
	finishTimeout = 5 * time.Second
)

// IdempotencyDecider описывает решения слоя идемпотентности (usecase).
type IdempotencyDecider interface {
	Begin(ctx context.Context, merchantID, key string, fingerprint []byte) (usecase.Decision, error)
	Finish(ctx context.Context, merchantID, key, attemptID string, fingerprint []byte, status int, contentType string, body []byte, overflow bool) error
}

// Idempotency возвращает middleware идемпотентности для POST-маршрутов:
// повтор с тем же телом отдает сохраненный ответ с Idempotent-Replayed,
// другое тело при том же ключе - 409, ошибки ключ не занимают. Применяется
// после аутентификации (мерчант из контекста).
func Idempotency(decider IdempotencyDecider) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}
		merchantID := MerchantID(c)
		if merchantID == "" {
			AbortUnauthorized(c)
			return
		}
		key, ok := idempotencyKey(c)
		if !ok {
			return
		}
		body, err := readLimitedBody(c)
		if err != nil {
			abortBodyError(c, err)
			return
		}
		fingerprint := domain.RequestFingerprint(c.Request.Method, c.Request.URL.Path, body)

		decision, err := decider.Begin(c.Request.Context(), merchantID, key, fingerprint)
		if err != nil {
			// Без журнала решений повтор неотличим от нового вызова: fail-closed.
			c.Header("Retry-After", "1")
			ginmw.AbortWithProblem(c, http.StatusServiceUnavailable, "service_unavailable",
				"Service Unavailable", "идемпотентность временно недоступна")
			return
		}
		switch decision.Kind {
		case usecase.DecisionReplay:
			replay(c, decision.Replay)
			return
		case usecase.DecisionConflict:
			abortIdempotencyError(c, decision.Reason)
			return
		}

		writer := &snapshotWriter{ResponseWriter: c.Writer}
		c.Writer = writer
		defer func() {
			// Фиксация выполняется независимо от отмены клиента: успешный
			// ответ не должен теряться из-за дисконнекта.
			finishCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), finishTimeout)
			defer cancel()
			status := writer.Status()
			if status == 0 {
				// Паника или пустой ответ: попытка освобождается.
				status = http.StatusInternalServerError
			}
			_ = decider.Finish(finishCtx, merchantID, key, decision.AttemptID, fingerprint,
				status, writer.Header().Get("Content-Type"), writer.Body(), writer.Overflow())
		}()
		c.Next()
	}
}

// idempotencyKey извлекает и валидирует единственный заголовок ключа.
func idempotencyKey(c *gin.Context) (string, bool) {
	values := c.Request.Header.Values(idempotencyHeader)
	if len(values) != 1 {
		// Отсутствие и дубли заголовка недопустимы: расхождение значений
		// с периметром исключено (как у Authorization).
		if len(values) == 0 {
			abortIdempotencyError(c, domain.ErrMissingIdempotencyKey)
		} else {
			abortIdempotencyError(c, domain.ErrInvalidIdempotencyKey)
		}
		return "", false
	}
	key := values[0]
	if err := domain.ValidateIdempotencyKey(key); err != nil {
		abortIdempotencyError(c, err)
		return "", false
	}
	return key, true
}

// readLimitedBody читает тело запроса с ограничением; тело восстанавливается
// для обработчика.
func readLimitedBody(c *gin.Context) ([]byte, error) {
	limited := http.MaxBytesReader(c.Writer, c.Request.Body, MaxIdempotencyRequestBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

// abortBodyError отвечает на ошибку чтения тела: превышение предела - 413,
// прочие сбои транспорта - 400.
func abortBodyError(c *gin.Context, err error) {
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		ginmw.AbortWithProblem(c, http.StatusRequestEntityTooLarge, "payload_too_large",
			"Payload Too Large", "тело запроса превышает предел")
		return
	}
	ginmw.AbortWithProblem(c, http.StatusBadRequest, "validation_failed",
		"Bad Request", "тело запроса не читается")
}

// replay отдает сохраненный ответ с признаком повтора.
func replay(c *gin.Context, snapshot usecase.Snapshot) {
	if snapshot.ContentType != "" {
		c.Header("Content-Type", snapshot.ContentType)
	}
	c.Header("Idempotent-Replayed", "true")
	c.Status(snapshot.Status)
	_, _ = c.Writer.Write(snapshot.Body)
	c.Abort()
}

// abortIdempotencyError маппит ошибки ключа в problem+json.
func abortIdempotencyError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, domain.ErrMissingIdempotencyKey):
		ginmw.AbortWithProblem(c, http.StatusBadRequest, "missing_idempotency_key",
			"Bad Request", "заголовок Idempotency-Key обязателен")
	case errors.Is(err, domain.ErrInvalidIdempotencyKey):
		ginmw.AbortWithProblem(c, http.StatusBadRequest, "validation_failed",
			"Bad Request", "Idempotency-Key имеет недопустимую форму")
	case errors.Is(err, domain.ErrIdempotencyKeyReuse):
		ginmw.AbortWithProblem(c, http.StatusConflict, "idempotency_key_reuse",
			"Conflict", "ключ идемпотентности уже использован с другим телом")
	case errors.Is(err, domain.ErrIdempotencyInProgress):
		ginmw.AbortWithProblem(c, http.StatusConflict, "idempotency_in_progress",
			"Conflict", "запрос с этим ключом уже обрабатывается")
	default:
		ginmw.AbortWithProblem(c, http.StatusInternalServerError, "internal",
			"Internal Server Error", "внутренняя ошибка")
	}
}

// snapshotWriter захватывает тело и статус ответа для сохранения снимка;
// тела сверх предела не буферизуются, но клиенту отдаются полностью.
type snapshotWriter struct {
	gin.ResponseWriter
	buf      bytes.Buffer
	overflow bool
}

// Write при необходимости запоминает тело ответа.
func (w *snapshotWriter) Write(data []byte) (int, error) {
	if !w.overflow && w.buf.Len()+len(data) <= usecase.MaxIdempotencyResponseBytes {
		w.buf.Write(data)
	} else {
		w.overflow = true
	}
	return w.ResponseWriter.Write(data)
}

// WriteString при необходимости запоминает тело ответа.
func (w *snapshotWriter) WriteString(s string) (int, error) {
	if !w.overflow && w.buf.Len()+len(s) <= usecase.MaxIdempotencyResponseBytes {
		w.buf.WriteString(s)
	} else {
		w.overflow = true
	}
	return w.ResponseWriter.WriteString(s)
}

// Body возвращает захваченное тело; при переполнении буфера пусто.
func (w *snapshotWriter) Body() []byte {
	if w.overflow {
		return nil
	}
	return w.buf.Bytes()
}

// Overflow сообщает, что ответ превысил предел снимка: он не сохраняется.
func (w *snapshotWriter) Overflow() bool {
	return w.overflow
}
