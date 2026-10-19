package transport

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/cbl/cbl-billing-api/internal/transport/httpmw"
	"github.com/cbl/cbl-billing-api/internal/usecase"
)

// StatementDeps groups the balances and ledger route dependencies.
type StatementDeps struct {
	// Statements is the statement service.
	Statements *usecase.StatementService
	// Log is the structured logger.
	Log *zap.Logger
}

// balanceItemDTO describes the contract BalanceItem.
type balanceItemDTO struct {
	// Currency sets the asset code.
	Currency string `json:"currency"`
	// Available sets the available balance decimal string.
	Available string `json:"available"`
	// Reserved sets the reserved balance decimal string.
	Reserved string `json:"reserved,omitempty"`
	// Pending sets the funds awaiting credit.
	Pending string `json:"pending,omitempty"`
}

// balanceListDTO describes the contract BalanceList.
type balanceListDTO struct {
	// Items sets the per-asset rows.
	Items []balanceItemDTO `json:"items"`
}

// ledgerEntryDTO describes the contract LedgerEntry.
type ledgerEntryDTO struct {
	// EntryID sets the monotonic entry identifier.
	EntryID int64 `json:"entry_id"`
	// AccountID sets the account identifier.
	AccountID string `json:"account_id"`
	// Direction sets the entry direction word.
	Direction string `json:"direction"`
	// Amount sets the entry amount.
	Amount *assetDTO `json:"amount"`
	// RefType sets the source operation type.
	RefType string `json:"ref_type,omitempty"`
	// RefID sets the source operation identifier.
	RefID string `json:"ref_id,omitempty"`
	// CreatedAt sets the posting time.
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// ledgerEntryListDTO describes the contract LedgerEntryList.
type ledgerEntryListDTO struct {
	// Items sets the page rows.
	Items []ledgerEntryDTO `json:"items"`
	// NextCursor sets the next page cursor.
	NextCursor string `json:"next_cursor,omitempty"`
}

// RegisterStatements registers GET /v1/balances and GET /v1/ledger/entries in
// the Require group.
func RegisterStatements(group *gin.RouterGroup, deps StatementDeps) {
	group.GET("/balances", func(c *gin.Context) {
		handleBalances(c, deps)
	})
	group.GET("/ledger/entries", func(c *gin.Context) {
		handleLedgerEntries(c, deps)
	})
}

// handleBalances serves GET /v1/balances.
func handleBalances(c *gin.Context, deps StatementDeps) {
	if deps.Statements == nil {
		abortDependency(c, "the statement service is not configured")
		return
	}
	merchantID := httpmw.MerchantID(c)
	if !validMerchantID(c, merchantID) {
		return
	}
	ctx, cancel := withBudget(c)
	defer cancel()
	items, err := deps.Statements.Balances(ctx, merchantID)
	if err != nil {
		abortInvoiceError(c, deps.Log, err)
		return
	}
	rows := make([]balanceItemDTO, 0, len(items))
	for _, item := range items {
		row := balanceItemDTO{
			Currency:  item.Currency,
			Available: formatMinor(item.Available.MinorUnits, item.Available.Decimals),
		}
		if item.Reserved.MinorUnits > 0 {
			row.Reserved = formatMinor(item.Reserved.MinorUnits, item.Reserved.Decimals)
		}
		if item.Pending.MinorUnits > 0 {
			row.Pending = formatMinor(item.Pending.MinorUnits, item.Pending.Decimals)
		}
		rows = append(rows, row)
	}
	c.JSON(http.StatusOK, balanceListDTO{Items: rows})
}

// handleLedgerEntries serves GET /v1/ledger/entries.
func handleLedgerEntries(c *gin.Context, deps StatementDeps) {
	if deps.Statements == nil {
		abortDependency(c, "the statement service is not configured")
		return
	}
	merchantID := httpmw.MerchantID(c)
	if !validMerchantID(c, merchantID) {
		return
	}
	query, err := parseEntriesQuery(c, merchantID)
	if err != nil {
		abortValidation(c, err.Error())
		return
	}
	ctx, cancel := withBudget(c)
	defer cancel()
	page, err := deps.Statements.Entries(ctx, query)
	if err != nil {
		abortInvoiceError(c, deps.Log, err)
		return
	}
	items := make([]ledgerEntryDTO, 0, len(page.Entries))
	for _, entry := range page.Entries {
		amount := entry.Amount
		items = append(items, ledgerEntryDTO{
			EntryID:   entry.EntryID,
			AccountID: entry.AccountID,
			Direction: entry.Direction,
			Amount:    moneyDTO(amount),
			RefType:   entry.RefType,
			RefID:     entry.RefID,
			CreatedAt: entry.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, ledgerEntryListDTO{Items: items, NextCursor: page.NextCursor})
}

// parseEntriesQuery parses and validates the statement parameters.
func parseEntriesQuery(c *gin.Context, merchantID string) (usecase.EntriesQuery, error) {
	query := usecase.EntriesQuery{MerchantID: merchantID, Limit: defaultInvoiceLimit}
	if raw := c.Query("account_id"); raw != "" {
		if !uuidPattern.MatchString(raw) {
			return usecase.EntriesQuery{}, errors.New("account_id must be a UUID")
		}
		query.AccountID = raw
	}
	if raw := c.Query("from"); raw != "" {
		value, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return usecase.EntriesQuery{}, errors.New("from must be RFC 3339")
		}
		query.From = &value
	}
	if raw := c.Query("to"); raw != "" {
		value, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return usecase.EntriesQuery{}, errors.New("to must be RFC 3339")
		}
		query.To = &value
	}
	if query.From != nil && query.To != nil && query.To.Before(*query.From) {
		return usecase.EntriesQuery{}, errors.New("to must not be before from")
	}
	if raw := c.Query("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxInvoiceLimit {
			return usecase.EntriesQuery{}, errors.New("limit must be an integer between 1 and 100")
		}
		query.Limit = limit
	}
	if raw := c.Query("cursor"); raw != "" {
		if len(raw) > maxCursorLength {
			return usecase.EntriesQuery{}, errors.New("cursor must be at most 512 characters")
		}
		query.Cursor = raw
	}
	return query, nil
}
