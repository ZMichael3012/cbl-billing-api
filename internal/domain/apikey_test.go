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
	require.NoError(t, err, "сгенерированный токен проходит собственный парсер")
	assert.Equal(t, prefix, parsedPrefix)
	assert.Len(t, prefix, 10)
	assert.True(t, bytes.Equal(hash, domain.HashToken(token)), "хэш соответствует полному токену")
	assert.False(t, bytes.Equal(hash, domain.HashToken(token+"x")), "хэш чувствителен к токену")
	assert.True(t, strings.HasPrefix(token, "cbl_v1_"+prefix+"_"))

	other, otherPrefix, otherHash, err := domain.GenerateToken()
	require.NoError(t, err)
	assert.NotEqual(t, token, other, "токены уникальны")
	assert.NotEqual(t, prefix, otherPrefix)
	assert.False(t, bytes.Equal(hash, otherHash))
}

func TestParseTokenRejections(t *testing.T) {
	t.Parallel()

	token, prefix, _, err := domain.GenerateToken()
	require.NoError(t, err)
	// Секрет может содержать "_" как символ base64url: граница - по схеме и prefix.
	secret := token[len("cbl_v1_")+len(prefix)+1:]

	cases := map[string]string{
		"пустой":             "",
		"без схемы":          token[len("cbl_v1_"):],
		"другая схема":       "cbl_v2_" + token[len("cbl_v1_"):],
		"верхний регистр":    strings.ToUpper(token),
		"короткий секрет":    "cbl_v1_0011223344_" + secret[:42],
		"длинный секрет":     "cbl_v1_0011223344_" + secret + "a",
		"не hex prefix":      "cbl_v1_zz11223344_" + secret,
		"короткий prefix":    "cbl_v1_00112233_" + secret,
		"лишний разделитель": "cbl_v1_0011223344_" + secret + "_x",
		"пробелы":            " cbl_v1_0011223344_" + secret,
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
		assert.True(t, domain.ValidMerchantID(ok), "ожидался допустимым: %q", ok)
	}
	for _, bad := range []string{"", " ", "m 1", "m:1", "м-1", strings.Repeat("a", 129)} {
		assert.False(t, domain.ValidMerchantID(bad), "ожидался недопустимым: %q", bad)
	}
}

func TestStatusValid(t *testing.T) {
	t.Parallel()

	assert.True(t, domain.StatusActive.Valid())
	assert.True(t, domain.StatusRetiring.Valid())
	assert.True(t, domain.StatusRevoked.Valid())
	assert.False(t, domain.Status("unknown").Valid())
}
