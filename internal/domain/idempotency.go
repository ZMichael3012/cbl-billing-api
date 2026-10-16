package domain

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
)

// idempotencyKeyPattern defines the Idempotency-Key header shape (contract 7.1).
var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{8,128}$`)

// Idempotency domain errors; the middleware maps them to Problem codes.
var (
	// ErrMissingIdempotencyKey reports a missing required header.
	ErrMissingIdempotencyKey = errors.New("idempotency: key is required")
	// ErrInvalidIdempotencyKey reports an invalid key shape.
	ErrInvalidIdempotencyKey = errors.New("idempotency: invalid key format")
	// ErrIdempotencyKeyReuse reports key reuse with a different body.
	ErrIdempotencyKeyReuse = errors.New("idempotency: key reuse with different request")
	// ErrIdempotencyInProgress reports concurrent processing of the same key.
	ErrIdempotencyInProgress = errors.New("idempotency: request with this key is in progress")
)

// ValidIdempotencyKey reports that the key shape is valid.
func ValidIdempotencyKey(key string) bool {
	return idempotencyKeyPattern.MatchString(key)
}

// RequestFingerprint computes the request fingerprint: sha256 of the method,
// path and raw body; the key scope (merchant) is handled by the unique index
// and is not part of the fingerprint, so that a retry with the same body and
// key by the same merchant is recognized regardless of field order (the body
// is hashed as is).
func RequestFingerprint(method, path string, body []byte) []byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte(method))
	_, _ = hash.Write([]byte("\n"))
	_, _ = hash.Write([]byte(path))
	_, _ = hash.Write([]byte("\n"))
	_, _ = hash.Write(body)
	return hash.Sum(nil)
}

// ValidateIdempotencyKey validates the key of an untrusted request.
func ValidateIdempotencyKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w", ErrMissingIdempotencyKey)
	}
	if !ValidIdempotencyKey(key) {
		return fmt.Errorf("%w: %q", ErrInvalidIdempotencyKey, key)
	}
	return nil
}
