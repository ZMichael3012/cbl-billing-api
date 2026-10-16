package domain_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-billing-api/internal/domain"
)

func TestGenerateToken(t *testing.T) {
	t.Parallel()

	token, prefix, hash, err := domain.GenerateToken()
	require.NoError(t, err)

	parsedPrefix, err := domain.ParseToken(token)
	require.NoError(t, err, "the generated token passes its own parser")
	assert.Equal(t, prefix, parsedPrefix)
	assert.Len(t, prefix, 10)
	assert.True(t, bytes.Equal(hash, domain.HashToken(token)), "the hash corresponds to the full token")
	assert.False(t, bytes.Equal(hash, domain.HashToken(token+"x")), "the hash is sensitive to the token")
	assert.True(t, strings.HasPrefix(token, "cbl_v1_"+prefix+"_"))

	other, otherPrefix, otherHash, err := domain.GenerateToken()
	require.NoError(t, err)
	assert.NotEqual(t, token, other, "tokens are unique")
	assert.NotEqual(t, prefix, otherPrefix)
	assert.False(t, bytes.Equal(hash, otherHash))
}

func TestParseTokenRejections(t *testing.T) {
	t.Parallel()

	token, prefix, _, err := domain.GenerateToken()
	require.NoError(t, err)
	// The secret may contain "_" as a base64url character: the boundary is by
	// the scheme and prefix.
	secret := token[len("cbl_v1_")+len(prefix)+1:]

	cases := map[string]string{
		"empty":           "",
		"no scheme":       token[len("cbl_v1_"):],
		"other scheme":    "cbl_v2_" + token[len("cbl_v1_"):],
		"uppercase":       strings.ToUpper(token),
		"short secret":    "cbl_v1_0011223344_" + secret[:42],
		"long secret":     "cbl_v1_0011223344_" + secret + "a",
		"non-hex prefix":  "cbl_v1_zz11223344_" + secret,
		"short prefix":    "cbl_v1_00112233_" + secret,
		"extra separator": "cbl_v1_0011223344_" + secret + "_x",
		"spaces":          " cbl_v1_0011223344_" + secret,
	}
	for name, value := range cases {
		_, err := domain.ParseToken(value)
		require.Error(t, err, name)
		assert.ErrorIs(t, err, domain.ErrUnauthorized, name)
	}
}

func TestValidMerchantID(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{"m-1", "merchant_01", "A", strings.Repeat("a", 128)} {
		assert.True(t, domain.ValidMerchantID(ok), "expected valid: %q", ok)
	}
	for _, bad := range []string{"", " ", "m 1", "m:1", "м-1", strings.Repeat("a", 129)} {
		assert.False(t, domain.ValidMerchantID(bad), "expected invalid: %q", bad)
	}
}

func TestStatusValid(t *testing.T) {
	t.Parallel()

	assert.True(t, domain.StatusActive.Valid())
	assert.True(t, domain.StatusRetiring.Valid())
	assert.True(t, domain.StatusRevoked.Valid())
	assert.False(t, domain.Status("unknown").Valid())
}
