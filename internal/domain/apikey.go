package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Parameters of the key format: cbl_v1_<prefix>_<secret>.
const (
	// tokenScheme defines the versioned key prefix: a scheme change is
	// released as a new version.
	tokenScheme = "cbl_v1_"
	// PrefixBytes defines the length of the random prefix part in bytes.
	PrefixBytes = 5
	// SecretBytes defines the length of the key secret in bytes (CSPRNG).
	SecretBytes = 32
)

// tokenPattern defines the strict key format: the prefix is 10 hex characters,
// the secret is 43 base64url characters without padding.
var tokenPattern = regexp.MustCompile(`^cbl_v1_([0-9a-f]{10})_([A-Za-z0-9_-]{43})$`)

// merchantIDPattern defines the merchant identifier shape: like the ledger
// account plan business key (CBL-003, section 9).
var merchantIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Key domain errors; transport and usecase map them to response codes.
var (
	// ErrUnauthorized reports an unknown, revoked or expired key.
	ErrUnauthorized = errors.New("apikeys: unauthorized")
	// ErrKeyNotFound reports a missing key by identifier or prefix.
	ErrKeyNotFound = errors.New("apikeys: key not found")
	// ErrKeyAlreadyActive reports an existing active key of the merchant.
	ErrKeyAlreadyActive = errors.New("apikeys: active key already exists")
	// ErrStatusConflict reports a concurrent key status change.
	ErrStatusConflict = errors.New("apikeys: key status conflict")
	// ErrInvalidMerchantID reports an invalid merchant identifier.
	ErrInvalidMerchantID = errors.New("apikeys: invalid merchant id")
)

// Status describes the key lifecycle state.
type Status string

// Key states.
const (
	// StatusActive defines the valid key (a single one per merchant).
	StatusActive Status = "active"
	// StatusRetiring defines a replaced key within the rotation overlap window.
	StatusRetiring Status = "retiring"
	// StatusRevoked defines a revoked key (terminal state).
	StatusRevoked Status = "revoked"
)

// Valid reports that the state belongs to the lifecycle.
func (s Status) Valid() bool {
	return s == StatusActive || s == StatusRetiring || s == StatusRevoked
}

// APIKey describes a key record; the raw secret is not stored in the record -
// only TokenHash (sha256 of the full key).
type APIKey struct {
	// ID defines the record UUIDv7.
	ID string
	// MerchantID defines the key owner.
	MerchantID string
	// Prefix defines the public key prefix for audit and lookup.
	Prefix string
	// TokenHash defines the sha256 of the full key.
	TokenHash []byte
	// Status defines the lifecycle state.
	Status Status
	// CreatedAt defines the issue time.
	CreatedAt time.Time
	// ExpiresAt defines the overlap window end for a retiring key (7.2).
	ExpiresAt *time.Time
	// RevokedAt defines the revocation time.
	RevokedAt *time.Time
	// LastUsedAt defines the last successful use (best-effort).
	LastUsedAt *time.Time
	// ExpiresValid reports that the retiring key has not expired at read time
	// (computed by the DB from the server now(), not by application clocks).
	ExpiresValid bool
}

// ValidMerchantID reports that the merchant identifier is valid.
func ValidMerchantID(merchantID string) bool {
	return merchantIDPattern.MatchString(merchantID)
}

// GenerateToken generates a new key: it returns the full token (shown only
// once), the prefix for the record and the sha256 hash of the token for
// storage.
func GenerateToken() (token, prefix string, hash []byte, err error) {
	prefixBytes := make([]byte, PrefixBytes)
	if _, err := rand.Read(prefixBytes); err != nil {
		return "", "", nil, fmt.Errorf("apikeys: generate prefix: %w", err)
	}
	secretBytes := make([]byte, SecretBytes)
	if _, err := rand.Read(secretBytes); err != nil {
		return "", "", nil, fmt.Errorf("apikeys: generate secret: %w", err)
	}
	prefix = hex.EncodeToString(prefixBytes)
	token = tokenScheme + prefix + "_" + base64.RawURLEncoding.EncodeToString(secretBytes)
	return token, prefix, HashToken(token), nil
}

// HashToken returns the sha256 of the full token: the single lookup key in the
// DB and in the Redis verification cache (contract with the perimeter, 7.5).
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// ParseToken validates the token format and returns its prefix.
func ParseToken(token string) (string, error) {
	matches := tokenPattern.FindStringSubmatch(token)
	if matches == nil {
		return "", fmt.Errorf("%w: token format", ErrUnauthorized)
	}
	return matches[1], nil
}
