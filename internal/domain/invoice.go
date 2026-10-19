// Package domain contains the billing-api domain model.
package domain

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PaymentState describes the payment state machine state as the merchant
// contract spells it (cf-3.1 InvoiceStatus); the BFF projects it without
// transformation.
type PaymentState string

// Payment states (the cf-3.1 InvoiceStatus values).
const (
	PaymentStateCreating   PaymentState = "creating"
	PaymentStateCreated    PaymentState = "created"
	PaymentStatePending    PaymentState = "pending"
	PaymentStateDetected   PaymentState = "detected"
	PaymentStateConfirming PaymentState = "confirming"
	PaymentStateCompleted  PaymentState = "completed"
	PaymentStateExpired    PaymentState = "expired"
	PaymentStateUnderpaid  PaymentState = "underpaid"
	PaymentStateOverpaid   PaymentState = "overpaid"
	PaymentStateFailed     PaymentState = "failed"
	PaymentStateCancelled  PaymentState = "cancelled"
)

// ErrUnknownPaymentState reports a payment state outside the contract list
// (the client must display unknown values as is; the BFF logs and keeps them).
var ErrUnknownPaymentState = errors.New("unknown payment state")

// PaymentStateFromWord maps a lowercase state word into a contract state.
func PaymentStateFromWord(word string) (PaymentState, error) {
	state := PaymentState(strings.ToLower(strings.TrimSpace(word)))
	switch state {
	case PaymentStateCreating, PaymentStateCreated, PaymentStatePending,
		PaymentStateDetected, PaymentStateConfirming, PaymentStateCompleted,
		PaymentStateExpired, PaymentStateUnderpaid, PaymentStateOverpaid,
		PaymentStateFailed, PaymentStateCancelled:
		return state, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownPaymentState, word)
	}
}

// Money is a decimal amount in asset minor units; float is forbidden.
type Money struct {
	// MinorUnits sets the amount in minor units.
	MinorUnits int64
	// Asset sets the asset code.
	Asset string
	// Decimals sets the asset scale.
	Decimals uint32
}

// TimelineItem is one payment state history record.
type TimelineItem struct {
	// State sets the state of the record.
	State PaymentState
	// OccurredAt sets the transition time.
	OccurredAt time.Time
}

// PaymentView describes the payment fields the merchant invoice projects.
type PaymentView struct {
	// PaymentID is the platform payment identifier.
	PaymentID string
	// MerchantID is the owning merchant.
	MerchantID string
	// State is the current state.
	State PaymentState
	// ExpectedAmount is the locked pay amount (created onward).
	ExpectedAmount *Money
	// ReceivedAmount is the detected amount when present.
	ReceivedAmount *Money
	// PayAddress is the deposit address of the address flow.
	PayAddress string
	// Network is the payment network.
	Network string
	// Rate is the locked quote rate.
	Rate string
	// LockedUntil is the quote lock deadline.
	LockedUntil *time.Time
	// Txid is the network transaction hash when detected.
	Txid string
	// Confirmations is the current confirmation count.
	Confirmations uint32
	// CreatedAt is the payment creation time.
	CreatedAt time.Time
	// ExpiresAt is the invoice deadline.
	ExpiresAt *time.Time
	// CheckoutURL is the hosted payment page when the provider flow is hosted.
	CheckoutURL string
	// Timeline is the state history.
	Timeline []TimelineItem
}

// InvoiceProjection is the BFF-owned merchant context of an invoice; the
// payment service does not store these fields (CBL-17.14, D1).
type InvoiceProjection struct {
	// PaymentID is the payment identifier (the invoice id).
	PaymentID string
	// MerchantID is the owning merchant from the token.
	MerchantID string
	// MerchantOrderID is the merchant order reference.
	MerchantOrderID string
	// Metadata is the merchant metadata (bounded).
	Metadata map[string]string
	// Amount is the requested invoice amount (nil for legacy rows).
	Amount *Money
	// CreatedAt is the projection creation time.
	CreatedAt time.Time
}

// InvoiceQuery filters a projection page (the order is created_at DESC,
// payment_id DESC; the cursor is exclusive).
type InvoiceQuery struct {
	// MerchantID is the owning merchant (mandatory).
	MerchantID string
	// MerchantOrderID filters by the order reference when set.
	MerchantOrderID string
	// Limit bounds the page (1..100).
	Limit int
	// AfterCreatedAt is the exclusive cursor time bound.
	AfterCreatedAt *time.Time
	// AfterPaymentID is the exclusive cursor id bound.
	AfterPaymentID string
}

// DerivedKey builds a downstream idempotency key (7.3 FR-5):
// f(merchant, Idempotency-Key, step) = "{merchant}:{key}:{step}".
func DerivedKey(merchantID, key, step string) string {
	return merchantID + ":" + key + ":" + step
}

// InvoiceCursor is the opaque BFF list cursor (the merchant_order_id branch).
type InvoiceCursor struct {
	// CreatedAt is the last row time.
	CreatedAt time.Time
	// PaymentID is the last row id.
	PaymentID string
}

// invoiceCursorPrefix marks BFF-issued cursors so foreign cursors fail closed.
const invoiceCursorPrefix = "cursor_v1_"

// EncodeInvoiceCursor encodes the cursor into the opaque contract form.
func EncodeInvoiceCursor(cursor InvoiceCursor) string {
	raw := cursor.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + cursor.PaymentID
	return invoiceCursorPrefix + base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeInvoiceCursor decodes a BFF cursor; a foreign or broken value fails.
func DecodeInvoiceCursor(raw string) (InvoiceCursor, error) {
	if !strings.HasPrefix(raw, invoiceCursorPrefix) {
		return InvoiceCursor{}, errors.New("invalid cursor")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, invoiceCursorPrefix))
	if err != nil {
		return InvoiceCursor{}, fmt.Errorf("invalid cursor: %w", err)
	}
	parts := strings.SplitN(string(decoded), "|", 2)
	if len(parts) != 2 {
		return InvoiceCursor{}, errors.New("invalid cursor")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return InvoiceCursor{}, errors.New("invalid cursor")
	}
	if _, err := uuid.Parse(parts[1]); err != nil {
		// A forged id would otherwise reach the uuid column cast and surface
		// as an internal error instead of a 400.
		return InvoiceCursor{}, errors.New("invalid cursor")
	}
	return InvoiceCursor{CreatedAt: createdAt, PaymentID: parts[1]}, nil
}

// fiatDecimals maps the supported invoice fiat currencies to their scale
// (2-decimal ISO-4217 majors; the full catalog arrives with the capabilities
// source, T-BA-13).
var fiatDecimals = map[string]uint32{
	"AUD": 2, "BRL": 2, "CAD": 2, "CHF": 2, "CZK": 2, "DKK": 2, "EUR": 2,
	"GBP": 2, "HKD": 2, "INR": 2, "MXN": 2, "NOK": 2, "NZD": 2, "PLN": 2,
	"SEK": 2, "SGD": 2, "TRY": 2, "USD": 2, "ZAR": 2,
}

// FiatDecimals returns the scale of a supported invoice fiat currency.
func FiatDecimals(currency string) (uint32, bool) {
	decimals, ok := fiatDecimals[strings.ToUpper(strings.TrimSpace(currency))]
	return decimals, ok
}
