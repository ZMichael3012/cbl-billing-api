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

// Idempotency middleware constants.
const (
	// idempotencyHeader sets the mandatory idempotency key header.
	idempotencyHeader = "Idempotency-Key"
	// MaxIdempotencyRequestBytes sets the mutating request body limit: memory
	// DoS protection (the response snapshot is limited separately, 256 KiB).
	MaxIdempotencyRequestBytes = 1 << 20
	// finishTimeout sets the response commit budget after the client cancels the request.
	finishTimeout = 5 * time.Second
)

// IdempotencyDecider describes the decisions of the idempotency layer (usecase).
type IdempotencyDecider interface {
	Begin(ctx context.Context, merchantID, key string, fingerprint []byte) (usecase.Decision, error)
	Finish(ctx context.Context, merchantID, key, attemptID string, fingerprint []byte, status int, contentType string, body []byte, overflow bool) error
}

// Idempotency returns the idempotency middleware for POST routes: a retry with
// the same body returns the saved response with Idempotent-Replayed, a
// different body with the same key gives 409, errors do not occupy the key. It
// is applied after authentication (the merchant comes from the context).
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
			// Without the decision journal, a retry is indistinguishable from a
			// new call: fail-closed.
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
			// The commit runs regardless of client cancellation: a successful
			// response must not be lost because of a disconnect.
			finishCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), finishTimeout)
			defer cancel()
			status := writer.Status()
			if status == 0 {
				// A panic or an empty response: the attempt is released.
				status = http.StatusInternalServerError
			}
			_ = decider.Finish(finishCtx, merchantID, key, decision.AttemptID, fingerprint,
				status, writer.Header().Get("Content-Type"), writer.Body(), writer.Overflow())
		}()
		c.Next()
	}
}

// idempotencyKey extracts and validates the single key header.
func idempotencyKey(c *gin.Context) (string, bool) {
	values := c.Request.Header.Values(idempotencyHeader)
	if len(values) != 1 {
		// A missing or duplicated header is not allowed: disagreement of values
		// with the perimeter is excluded (as with Authorization).
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

// readLimitedBody reads the request body with a limit; the body is restored
// for the handler.
func readLimitedBody(c *gin.Context) ([]byte, error) {
	limited := http.MaxBytesReader(c.Writer, c.Request.Body, MaxIdempotencyRequestBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

// abortBodyError responds to a body read error: exceeding the limit gives 413,
// other transport failures give 400.
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

// replay returns the saved response with the replay marker.
func replay(c *gin.Context, snapshot usecase.Snapshot) {
	if snapshot.ContentType != "" {
		c.Header("Content-Type", snapshot.ContentType)
	}
	c.Header("Idempotent-Replayed", "true")
	c.Status(snapshot.Status)
	_, _ = c.Writer.Write(snapshot.Body)
	c.Abort()
}

// abortIdempotencyError maps key errors to problem+json.
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

// snapshotWriter captures the response body and status to save a snapshot;
// bodies above the limit are not buffered but are fully returned to the client.
type snapshotWriter struct {
	gin.ResponseWriter
	buf      bytes.Buffer
	overflow bool
}

// Write remembers the response body when needed.
func (w *snapshotWriter) Write(data []byte) (int, error) {
	if !w.overflow && w.buf.Len()+len(data) <= usecase.MaxIdempotencyResponseBytes {
		w.buf.Write(data)
	} else {
		w.overflow = true
	}
	return w.ResponseWriter.Write(data)
}

// WriteString remembers the response body when needed.
func (w *snapshotWriter) WriteString(s string) (int, error) {
	if !w.overflow && w.buf.Len()+len(s) <= usecase.MaxIdempotencyResponseBytes {
		w.buf.WriteString(s)
	} else {
		w.overflow = true
	}
	return w.ResponseWriter.WriteString(s)
}

// Body returns the captured body; it is empty when the buffer overflowed.
func (w *snapshotWriter) Body() []byte {
	if w.overflow {
		return nil
	}
	return w.buf.Bytes()
}

// Overflow reports that the response exceeded the snapshot limit: it is not saved.
func (w *snapshotWriter) Overflow() bool {
	return w.overflow
}
