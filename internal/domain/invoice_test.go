package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-billing-api/internal/domain"
)

func TestPaymentStateFromWord(t *testing.T) {
	state, err := domain.PaymentStateFromWord("COMPLETED")
	require.NoError(t, err)
	assert.Equal(t, domain.PaymentStateCompleted, state)

	state, err = domain.PaymentStateFromWord(" creating ")
	require.NoError(t, err)
	assert.Equal(t, domain.PaymentStateCreating, state)

	_, err = domain.PaymentStateFromWord("weird")
	require.ErrorIs(t, err, domain.ErrUnknownPaymentState)
}

func TestDerivedKey(t *testing.T) {
	key := domain.DerivedKey("m-1", "idem-123", "create_payment")
	assert.Equal(t, "m-1:idem-123:create_payment", key)
}

func TestInvoiceCursorRoundTrip(t *testing.T) {
	cursor := domain.InvoiceCursor{
		CreatedAt: time.Date(2026, 10, 19, 8, 30, 0, 123456789, time.UTC),
		PaymentID: "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b",
	}
	encoded := domain.EncodeInvoiceCursor(cursor)
	assert.NotEqual(t, "", encoded)

	decoded, err := domain.DecodeInvoiceCursor(encoded)
	require.NoError(t, err)
	assert.True(t, cursor.CreatedAt.Equal(decoded.CreatedAt))
	assert.Equal(t, cursor.PaymentID, decoded.PaymentID)
}

func TestInvoiceCursorRejectsForeignAndBroken(t *testing.T) {
	_, err := domain.DecodeInvoiceCursor("some-foreign-cursor")
	require.Error(t, err)
	_, err = domain.DecodeInvoiceCursor("cursor_v1_%%%")
	require.Error(t, err)
	_, err = domain.DecodeInvoiceCursor("cursor_v1_" + "bm90LWEtcGFpcg")
	require.Error(t, err)
}

func TestFiatDecimals(t *testing.T) {
	decimals, ok := domain.FiatDecimals("usd")
	require.True(t, ok)
	assert.Equal(t, uint32(2), decimals)
	_, ok = domain.FiatDecimals("JPY")
	assert.False(t, ok, "currencies with a non-2 scale are outside the catalog until T-BA-13")
}
