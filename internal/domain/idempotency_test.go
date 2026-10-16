package domain_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-billing-api/internal/domain"
)

func TestRequestFingerprint(t *testing.T) {
	t.Parallel()

	base := domain.RequestFingerprint("POST", "/v1/invoices", []byte(`{"a":1}`))
	require.Len(t, base, 32)

	assert.True(t, bytes.Equal(base, domain.RequestFingerprint("POST", "/v1/invoices", []byte(`{"a":1}`))),
		"the fingerprint is deterministic")
	assert.False(t, bytes.Equal(base, domain.RequestFingerprint("POST", "/v1/invoices", []byte(`{"a":2}`))),
		"a different body yields a different fingerprint")
	assert.False(t, bytes.Equal(base, domain.RequestFingerprint("POST", "/v1/payouts", []byte(`{"a":1}`))),
		"a different path yields a different fingerprint")
	assert.False(t, bytes.Equal(base, domain.RequestFingerprint("PUT", "/v1/invoices", []byte(`{"a":1}`))),
		"a different method yields a different fingerprint")
	assert.False(t, bytes.Equal(base, domain.RequestFingerprint("POST", "/v1/invoices", []byte(`{"a":1} `))),
		"a trailing space in the body matters (raw body hash)")
}

func TestValidateIdempotencyKey(t *testing.T) {
	t.Parallel()

	require.NoError(t, domain.ValidateIdempotencyKey("order-12345678"))
	require.NoError(t, domain.ValidateIdempotencyKey(strings.Repeat("a", 128)))

	err := domain.ValidateIdempotencyKey("")
	require.ErrorIs(t, err, domain.ErrMissingIdempotencyKey)

	for _, invalid := range []string{
		"short7c", // fewer than 8 characters
		strings.Repeat("a", 129),
		"key with space",
		"key/slash",
		"ключ-кириллица",
	} {
		err := domain.ValidateIdempotencyKey(invalid)
		require.ErrorIs(t, err, domain.ErrInvalidIdempotencyKey, "key %q", invalid)
	}
}
