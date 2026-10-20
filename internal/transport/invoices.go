package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/ZMichael3012/cbl-kit/ginmw"
	kitmoney "github.com/ZMichael3012/cbl-kit/money"

	"github.com/cbl/cbl-billing-api/internal/connector"
	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/ledger"
	"github.com/cbl/cbl-billing-api/internal/payment"
	"github.com/cbl/cbl-billing-api/internal/storage"
	"github.com/cbl/cbl-billing-api/internal/transport/httpmw"
	"github.com/cbl/cbl-billing-api/internal/usecase"
)

// Merchant API invoice limits (openapi/billing.v1.yaml).
const (
	defaultInvoiceLimit = 20
	maxInvoiceLimit     = 100
	minExpiresInSec     = 60
	maxExpiresInSec     = 86400
	defaultExpiresInSec = 900
	maxOrderIDLength    = 128
	maxMetadataKeys     = 32
	maxMetadataKeyLen   = 64
	maxMetadataValueLen = 512
	maxMoneyLength      = 40
)

// uuidV7Pattern matches the contract invoice identifier (UUIDv7).
var uuidV7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// uuidPattern matches the contract account identifier (any UUID version).
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// requestBudget bounds all downstream calls of one request: the fan-out reads
// (the pending scan, the order-id hydration) must fail with 503 well below the
// server write timeout instead of pinning the handler (security review).
const requestBudget = 10 * time.Second

// withBudget derives the request context with the shared downstream budget.
func withBudget(c *gin.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), requestBudget)
}

// moneyPattern matches the contract decimal money string.
var moneyPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// CapabilitiesClient describes the connector capabilities dependency used for
// the create pre-check (decimals and amount bounds; CBL-17.14).
type CapabilitiesClient interface {
	// GetCapabilities returns the configured provider capability rows.
	GetCapabilities(ctx context.Context) ([]connector.Capability, error)
}

// InvoicesDeps groups the invoice route dependencies.
type InvoicesDeps struct {
	// Invoices is the invoice service.
	Invoices *usecase.InvoiceService
	// Capabilities resolves asset scales and amount bounds (optional).
	Capabilities CapabilitiesClient
	// Log is the structured logger.
	Log *zap.Logger
}

// invoiceCreateRequest describes the contract InvoiceCreateRequest.
type invoiceCreateRequest struct {
	// MerchantOrderID sets the merchant order reference.
	MerchantOrderID string `json:"merchant_order_id"`
	// Amount sets the invoice amount as a decimal string.
	Amount string `json:"amount"`
	// Currency sets the invoice currency (fiat or asset).
	Currency string `json:"currency"`
	// PayCurrency sets the payment asset.
	PayCurrency string `json:"pay_currency"`
	// Network sets the payment network.
	Network string `json:"network"`
	// ExpiresInSec sets the invoice lifetime in seconds.
	ExpiresInSec *int `json:"expires_in_sec"`
	// Metadata sets the bounded merchant metadata.
	Metadata map[string]string `json:"metadata"`
}

// assetDTO describes the contract Asset.
type assetDTO struct {
	// Amount sets the decimal amount string.
	Amount string `json:"amount"`
	// Currency sets the asset code.
	Currency string `json:"currency"`
}

// quoteDTO describes the contract Quote.
type quoteDTO struct {
	// Rate sets the locked rate decimal string.
	Rate string `json:"rate"`
	// LockedUntil sets the quote lock deadline.
	LockedUntil *time.Time `json:"locked_until,omitempty"`
}

// timelineDTO describes the contract TimelineItem.
type timelineDTO struct {
	// State sets the state word.
	State string `json:"state"`
	// OccurredAt sets the transition time.
	OccurredAt time.Time `json:"occurred_at"`
}

// invoiceDTO describes the contract Invoice.
type invoiceDTO struct {
	// ID sets the invoice identifier.
	ID string `json:"id"`
	// MerchantOrderID sets the merchant order reference.
	MerchantOrderID string `json:"merchant_order_id,omitempty"`
	// Status sets the invoice state.
	Status string `json:"status"`
	// Amount sets the invoice amount.
	Amount *assetDTO `json:"amount,omitempty"`
	// PayAmount sets the locked pay amount.
	PayAmount *assetDTO `json:"pay_amount,omitempty"`
	// PayAddress sets the deposit address of the address flow.
	PayAddress string `json:"pay_address,omitempty"`
	// Network sets the payment network.
	Network string `json:"network,omitempty"`
	// PaidAmount sets the detected amount.
	PaidAmount *assetDTO `json:"paid_amount,omitempty"`
	// Confirmations sets the confirmation count.
	Confirmations *uint32 `json:"confirmations,omitempty"`
	// Txid sets the network transaction hash.
	Txid string `json:"txid,omitempty"`
	// Quote sets the locked rate quote.
	Quote *quoteDTO `json:"quote,omitempty"`
	// ExpiresAt sets the invoice deadline.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// CheckoutURL sets the hosted payment page.
	CheckoutURL string `json:"checkout_url,omitempty"`
	// Timeline sets the state history.
	Timeline []timelineDTO `json:"timeline,omitempty"`
	// Metadata sets the merchant metadata.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// invoiceListDTO describes the contract InvoiceList.
type invoiceListDTO struct {
	// Items sets the page rows.
	Items []invoiceDTO `json:"items"`
	// NextCursor sets the next page cursor.
	NextCursor string `json:"next_cursor,omitempty"`
}

// RegisterInvoices registers the invoice routes in the Require group: POST
// /v1/invoices (with the idempotency middleware), GET /v1/invoices/{id} and
// GET /v1/invoices.
func RegisterInvoices(group *gin.RouterGroup, deps InvoicesDeps) {
	group.POST("/invoices", func(c *gin.Context) {
		handleCreateInvoice(c, deps)
	})
	group.GET("/invoices", func(c *gin.Context) {
		handleListInvoices(c, deps)
	})
	group.GET("/invoices/:id", func(c *gin.Context) {
		handleGetInvoice(c, deps)
	})
}

// handleCreateInvoice serves POST /v1/invoices.
func handleCreateInvoice(c *gin.Context, deps InvoicesDeps) {
	if deps.Invoices == nil {
		abortDependency(c, "the invoice service is not configured")
		return
	}
	merchantID := httpmw.MerchantID(c)
	if !validMerchantID(c, merchantID) {
		return
	}
	var request invoiceCreateRequest
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		abortValidation(c, "invalid request body: "+err.Error())
		return
	}
	idempotencyKey := c.GetHeader("Idempotency-Key")
	if idempotencyKey == "" {
		// The middleware rejects a missing key before the handler; the guard
		// protects a misconfigured chain.
		abortValidation(c, "Idempotency-Key is required")
		return
	}
	if deps.Capabilities == nil {
		abortDependency(c, "connector capabilities are not configured")
		return
	}
	input, problem := buildCreateInput(c, request, merchantID, idempotencyKey, deps)
	if problem != "" {
		return
	}
	ctx, cancel := withBudget(c)
	defer cancel()
	invoice, err := deps.Invoices.Create(ctx, input)
	if err != nil {
		abortInvoiceError(c, deps.Log, err)
		return
	}
	c.JSON(http.StatusCreated, invoiceToDTO(invoice))
}

// buildCreateInput validates the request against the capabilities and builds
// the service input; a non-empty problem means the response is already sent.
func buildCreateInput(c *gin.Context, request invoiceCreateRequest, merchantID, idempotencyKey string, deps InvoicesDeps) (usecase.CreateInvoiceInput, string) {
	if request.MerchantOrderID == "" || len(request.MerchantOrderID) > maxOrderIDLength {
		abortValidation(c, "merchant_order_id is required and must be at most 128 characters")
		return usecase.CreateInvoiceInput{}, "sent"
	}
	if request.Currency == "" || request.PayCurrency == "" || request.Network == "" {
		abortValidation(c, "currency, pay_currency and network are required")
		return usecase.CreateInvoiceInput{}, "sent"
	}
	if err := validateMetadata(request.Metadata); err != nil {
		abortValidation(c, err.Error())
		return usecase.CreateInvoiceInput{}, "sent"
	}
	expiresInSec := defaultExpiresInSec
	if request.ExpiresInSec != nil {
		expiresInSec = *request.ExpiresInSec
		if expiresInSec < minExpiresInSec || expiresInSec > maxExpiresInSec {
			abortValidation(c, fmt.Sprintf("expires_in_sec must be between %d and %d", minExpiresInSec, maxExpiresInSec))
			return usecase.CreateInvoiceInput{}, "sent"
		}
	}
	rows, err := deps.Capabilities.GetCapabilities(c.Request.Context())
	if err != nil {
		abortDependency(c, "capabilities are unavailable")
		return usecase.CreateInvoiceInput{}, "sent"
	}
	payRows := filterCapabilities(rows, request.PayCurrency, request.Network)
	if len(payRows) == 0 {
		ginmw.AbortWithProblem(c, http.StatusUnprocessableEntity, "asset_network_unsupported",
			"Unprocessable Entity", "the asset and network are not supported by any provider")
		return usecase.CreateInvoiceInput{}, "sent"
	}
	decimals, ok := resolveDecimals(rows, request.Currency, request.PayCurrency, payRows)
	if !ok {
		ginmw.AbortWithProblem(c, http.StatusUnprocessableEntity, "currency_unsupported",
			"Unprocessable Entity", "the invoice currency is outside the supported catalog")
		return usecase.CreateInvoiceInput{}, "sent"
	}
	amount, err := parseMoney(request.Amount, request.Currency, decimals)
	if err != nil {
		abortValidation(c, err.Error())
		return usecase.CreateInvoiceInput{}, "sent"
	}
	if request.Currency == request.PayCurrency {
		if problem := checkAmountBounds(c, amount, payRows); problem != "" {
			return usecase.CreateInvoiceInput{}, problem
		}
	}
	return usecase.CreateInvoiceInput{
		MerchantID:      merchantID,
		IdempotencyKey:  idempotencyKey,
		MerchantOrderID: request.MerchantOrderID,
		Amount:          amount,
		PayAsset:        request.PayCurrency,
		Network:         request.Network,
		ExpiresInSec:    expiresInSec,
		Metadata:        request.Metadata,
	}, ""
}

// handleGetInvoice serves GET /v1/invoices/{id}.
func handleGetInvoice(c *gin.Context, deps InvoicesDeps) {
	if deps.Invoices == nil {
		abortDependency(c, "the invoice service is not configured")
		return
	}
	merchantID := httpmw.MerchantID(c)
	if !validMerchantID(c, merchantID) {
		return
	}
	id := c.Param("id")
	if !uuidV7Pattern.MatchString(id) {
		abortValidation(c, "id must be a UUIDv7")
		return
	}
	ctx, cancel := withBudget(c)
	defer cancel()
	invoice, err := deps.Invoices.Get(ctx, merchantID, id)
	if err != nil {
		abortInvoiceError(c, deps.Log, err)
		return
	}
	c.JSON(http.StatusOK, invoiceToDTO(invoice))
}

// handleListInvoices serves GET /v1/invoices.
func handleListInvoices(c *gin.Context, deps InvoicesDeps) {
	if deps.Invoices == nil {
		abortDependency(c, "the invoice service is not configured")
		return
	}
	merchantID := httpmw.MerchantID(c)
	if !validMerchantID(c, merchantID) {
		return
	}
	query, err := parseInvoiceListQuery(c, merchantID)
	if err != nil {
		abortValidation(c, err.Error())
		return
	}
	ctx, cancel := withBudget(c)
	defer cancel()
	page, err := deps.Invoices.List(ctx, query)
	if err != nil {
		abortInvoiceError(c, deps.Log, err)
		return
	}
	items := make([]invoiceDTO, 0, len(page.Invoices))
	for _, invoice := range page.Invoices {
		items = append(items, invoiceToDTO(invoice))
	}
	c.JSON(http.StatusOK, invoiceListDTO{Items: items, NextCursor: page.NextCursor})
}

// parseInvoiceListQuery parses and validates the list parameters.
func parseInvoiceListQuery(c *gin.Context, merchantID string) (usecase.ListInvoicesQuery, error) {
	query := usecase.ListInvoicesQuery{MerchantID: merchantID, Limit: defaultInvoiceLimit}
	if raw := c.Query("status"); raw != "" {
		state, err := domain.PaymentStateFromWord(raw)
		if err != nil {
			return usecase.ListInvoicesQuery{}, errors.New("status is not a known invoice state")
		}
		query.Status = state
	}
	if raw := c.Query("from"); raw != "" {
		value, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return usecase.ListInvoicesQuery{}, errors.New("from must be RFC 3339")
		}
		query.From = &value
	}
	if raw := c.Query("to"); raw != "" {
		value, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return usecase.ListInvoicesQuery{}, errors.New("to must be RFC 3339")
		}
		query.To = &value
	}
	if query.From != nil && query.To != nil && query.To.Before(*query.From) {
		return usecase.ListInvoicesQuery{}, errors.New("to must not be before from")
	}
	if raw := c.Query("merchant_order_id"); raw != "" {
		if len(raw) > maxOrderIDLength {
			return usecase.ListInvoicesQuery{}, errors.New("merchant_order_id must be at most 128 characters")
		}
		query.MerchantOrderID = raw
	}
	if raw := c.Query("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxInvoiceLimit {
			return usecase.ListInvoicesQuery{}, fmt.Errorf("limit must be an integer between 1 and %d", maxInvoiceLimit)
		}
		query.Limit = limit
	}
	if raw := c.Query("cursor"); raw != "" {
		if len(raw) > maxCursorLength {
			return usecase.ListInvoicesQuery{}, fmt.Errorf("cursor must be at most %d characters", maxCursorLength)
		}
		query.Cursor = raw
	}
	return query, nil
}

// invoiceToDTO projects the aggregate into the contract shape.
func invoiceToDTO(invoice usecase.Invoice) invoiceDTO {
	view := invoice.Payment
	dto := invoiceDTO{
		ID:              view.PaymentID,
		MerchantOrderID: invoice.Projection.MerchantOrderID,
		Status:          string(view.State),
		PayAddress:      view.PayAddress,
		Network:         view.Network,
		Txid:            view.Txid,
		CheckoutURL:     view.CheckoutURL,
		Metadata:        invoice.Projection.Metadata,
	}
	if view.State == "" {
		dto.Status = string(domain.PaymentStateCreating)
	}
	if amount := invoice.Projection.Amount; amount != nil && amount.Asset != "" {
		dto.Amount = moneyDTO(*amount)
	}
	if amount := view.ExpectedAmount; amount != nil && view.State != domain.PaymentStateCreating {
		// KI-payment-11: the creating placeholder is not the locked amount; the
		// locked pay amount is rendered from created onward.
		dto.PayAmount = moneyDTO(*amount)
	}
	if received := view.ReceivedAmount; received != nil && received.MinorUnits > 0 {
		dto.PaidAmount = moneyDTO(*received)
	}
	if view.Rate != "" && view.LockedUntil != nil {
		// The contract Quote requires both fields; a rate without a lock
		// deadline would violate the schema.
		dto.Quote = &quoteDTO{Rate: view.Rate, LockedUntil: view.LockedUntil}
	}
	if view.ExpiresAt != nil {
		dto.ExpiresAt = view.ExpiresAt
	}
	if view.Confirmations > 0 {
		confirmations := view.Confirmations
		dto.Confirmations = &confirmations
	}
	for _, item := range view.Timeline {
		dto.Timeline = append(dto.Timeline, timelineDTO{State: string(item.State), OccurredAt: item.OccurredAt})
	}
	return dto
}

// moneyDTO formats an amount into the contract Asset.
func moneyDTO(amount domain.Money) *assetDTO {
	return &assetDTO{Amount: formatMinor(amount.MinorUnits, amount.Decimals), Currency: amount.Asset}
}

// formatMinor renders minor units as a decimal string without float; the
// magnitude is converted through uint64 so math.MinInt64 renders correctly and
// the scale is bounded by the contract maximum.
func formatMinor(minorUnits int64, decimals uint32) string {
	if decimals > kitmoney.MaxDecimals {
		decimals = kitmoney.MaxDecimals
	}
	if decimals == 0 {
		return strconv.FormatInt(minorUnits, 10)
	}
	negative := minorUnits < 0
	magnitude := uint64(minorUnits) //nolint:gosec // the two's-complement magnitude is corrected for negatives below
	if negative {
		magnitude = uint64(-(minorUnits + 1)) + 1 //nolint:gosec // the MinInt64-safe magnitude
	}
	digits := strconv.FormatUint(magnitude, 10)
	for len(digits) <= int(decimals) {
		digits = "0" + digits
	}
	cut := len(digits) - int(decimals)
	formatted := digits[:cut] + "." + digits[cut:]
	if negative {
		formatted = "-" + formatted
	}
	return formatted
}

// parseMoney validates and parses the contract money string.
func parseMoney(raw, currency string, decimals uint32) (domain.Money, error) {
	if raw == "" || len(raw) > maxMoneyLength || !moneyPattern.MatchString(raw) {
		return domain.Money{}, errors.New("amount must be a non-negative decimal string of at most 40 characters")
	}
	parsed, err := kitmoney.Parse(raw, currency, decimals)
	if err != nil {
		return domain.Money{}, fmt.Errorf("amount is not a valid %s value: %w", currency, err)
	}
	if parsed.MinorUnits <= 0 {
		return domain.Money{}, errors.New("amount must be greater than zero")
	}
	return domain.Money{MinorUnits: parsed.MinorUnits, Asset: currency, Decimals: decimals}, nil
}

// validateMetadata checks the metadata bounds of the contract.
func validateMetadata(metadata map[string]string) error {
	if len(metadata) > maxMetadataKeys {
		return fmt.Errorf("metadata must have at most %d keys", maxMetadataKeys)
	}
	for key, value := range metadata {
		if key == "" || len(key) > maxMetadataKeyLen {
			return fmt.Errorf("metadata keys must be 1..%d characters", maxMetadataKeyLen)
		}
		if len(value) > maxMetadataValueLen {
			return fmt.Errorf("metadata values must be at most %d characters", maxMetadataValueLen)
		}
	}
	return nil
}

// filterCapabilities returns the rows matching the asset and network.
func filterCapabilities(rows []connector.Capability, asset, network string) []connector.Capability {
	matches := make([]connector.Capability, 0, len(rows))
	for _, row := range rows {
		if row.Asset == asset && row.Network == network {
			matches = append(matches, row)
		}
	}
	return matches
}

// resolveDecimals finds the scale of the invoice currency: the capability
// scale of the asset, the pay asset scale or the fiat catalog.
func resolveDecimals(rows []connector.Capability, currency, payCurrency string, payRows []connector.Capability) (uint32, bool) {
	if currency == payCurrency {
		return payRows[0].Decimals, true
	}
	for _, row := range rows {
		if row.Asset == currency {
			return row.Decimals, true
		}
	}
	return domain.FiatDecimals(currency)
}

// checkAmountBounds pre-checks the amount against the capability bounds: the
// amount is accepted when at least one capability row accepts it (the envelope
// of the routing decision, D7); the routed provider still enforces its own
// limits and its rejection maps to the dependency problem.
func checkAmountBounds(c *gin.Context, amount domain.Money, rows []connector.Capability) string {
	accepted := false
	for _, row := range rows {
		aboveMin := row.MinAmount == nil || row.MinAmount.MinorUnits <= 0 || amount.MinorUnits >= row.MinAmount.MinorUnits
		belowMax := row.MaxAmount == nil || row.MaxAmount.MinorUnits <= 0 || amount.MinorUnits <= row.MaxAmount.MinorUnits
		if aboveMin && belowMax {
			accepted = true
			break
		}
	}
	if !accepted {
		ginmw.AbortWithProblem(c, http.StatusUnprocessableEntity, "amount_out_of_bounds",
			"Unprocessable Entity", "the amount is outside the provider bounds")
		return "sent"
	}
	return ""
}

// validMerchantID guards the token subject shape (D2: keys carry a UUID
// merchant id; a legacy non-UUID key is a configuration error).
func validMerchantID(c *gin.Context, merchantID string) bool {
	if merchantID == "" || !uuidPattern.MatchString(merchantID) {
		ginmw.AbortWithProblem(c, http.StatusForbidden, "invalid_merchant",
			"Forbidden", "the merchant identity is not a platform merchant id")
		return false
	}
	return true
}

// abortValidation responds 400 validation_failed.
func abortValidation(c *gin.Context, detail string) {
	ginmw.AbortWithProblem(c, http.StatusBadRequest, "validation_failed", "Validation Failed", detail)
}

// abortDependency responds 503 service_unavailable with a retry.
func abortDependency(c *gin.Context, detail string) {
	c.Header("Retry-After", "1")
	ginmw.AbortWithProblem(c, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable", detail)
}

// abortInvoiceError maps the dependency sentinels into problem+json. The
// details are fixed strings: downstream error texts may carry internal
// topology and never reach the merchant (the raw error goes to the log).
func abortInvoiceError(c *gin.Context, log *zap.Logger, err error) {
	if log != nil {
		log.Warn("invoice route dependency error",
			zap.String("request_id", ginmw.RequestIDFrom(c)), zap.Error(err))
	}
	switch {
	case errors.Is(err, usecase.ErrInvalidCursor):
		abortValidation(c, "the cursor is invalid or foreign")
	case errors.Is(err, payment.ErrInvalid), errors.Is(err, ledger.ErrInvalid):
		abortValidation(c, "the dependency rejected the request as malformed")
	case errors.Is(err, payment.ErrNotFound), errors.Is(err, ledger.ErrNotFound),
		errors.Is(err, storage.ErrNotFound), errors.Is(err, storage.ErrForeign):
		ginmw.AbortWithProblem(c, http.StatusNotFound, "not_found", "Not Found", "the resource was not found")
	case errors.Is(err, payment.ErrConflict):
		ginmw.AbortWithProblem(c, http.StatusConflict, "idempotency_key_reuse", "Conflict",
			"the idempotency key is bound to a different request")
	case errors.Is(err, payment.ErrRejected), errors.Is(err, ledger.ErrRejected):
		ginmw.AbortWithProblem(c, http.StatusUnprocessableEntity, "validation_failed", "Unprocessable Entity",
			"the dependency rejected the request")
	case errors.Is(err, payment.ErrUnavailable), errors.Is(err, ledger.ErrUnavailable), errors.Is(err, connector.ErrUnavailable):
		abortDependency(c, "a dependency is temporarily unavailable")
	default:
		abortDependency(c, "internal dependency failure")
	}
}
