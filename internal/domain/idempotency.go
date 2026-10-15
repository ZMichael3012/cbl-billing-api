package domain

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
)

// idempotencyKeyPattern задает форму заголовка Idempotency-Key (контракт 7.1).
var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{8,128}$`)

// Ошибки домена идемпотентности; middleware маппит их в коды Problem.
var (
	// ErrMissingIdempotencyKey сообщает об отсутствии обязательного заголовка.
	ErrMissingIdempotencyKey = errors.New("idempotency: key is required")
	// ErrInvalidIdempotencyKey сообщает о недопустимой форме ключа.
	ErrInvalidIdempotencyKey = errors.New("idempotency: invalid key format")
	// ErrIdempotencyKeyReuse сообщает о переиспользовании ключа с другим телом.
	ErrIdempotencyKeyReuse = errors.New("idempotency: key reuse with different request")
	// ErrIdempotencyInProgress сообщает о конкурентной обработке того же ключа.
	ErrIdempotencyInProgress = errors.New("idempotency: request with this key is in progress")
)

// ValidIdempotencyKey сообщает, что ключ допустим по форме.
func ValidIdempotencyKey(key string) bool {
	return idempotencyKeyPattern.MatchString(key)
}

// RequestFingerprint вычисляет отпечаток запроса: sha256 от метода, пути и
// сырого тела; область ключа (мерчант) учитывается уникальным индексом и не
// входит в отпечаток, чтобы повтор с тем же телом и ключом у того же мерчанта
// распознавался независимо от порядка полей (тело хэшируется как есть).
func RequestFingerprint(method, path string, body []byte) []byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte(method))
	_, _ = hash.Write([]byte("\n"))
	_, _ = hash.Write([]byte(path))
	_, _ = hash.Write([]byte("\n"))
	_, _ = hash.Write(body)
	return hash.Sum(nil)
}

// ValidateIdempotencyKey проверяет ключ недоверенного запроса.
func ValidateIdempotencyKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w", ErrMissingIdempotencyKey)
	}
	if !ValidIdempotencyKey(key) {
		return fmt.Errorf("%w: %q", ErrInvalidIdempotencyKey, key)
	}
	return nil
}
