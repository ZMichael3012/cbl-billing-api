package usecase

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/ledger"
	"github.com/cbl/cbl-billing-api/internal/payment"
)

// Pending aggregation bounds: the pending balance is a Payment aggregation
// (cf-3.1; the Ledger has no pending source), scanned over the detected and
// confirming states with a hard page cap (CBL-17.14, D4).
const (
	pendingPageSize = 200
	pendingPageCap  = 20
)

// LedgerClient describes the Ledger dependency of the statement service.
type LedgerClient interface {
	// ListMerchantAccounts returns the merchant accounts with balances.
	ListMerchantAccounts(ctx context.Context, merchantRef string) ([]ledger.MerchantAccount, error)
	// ListEntries returns a statement page.
	ListEntries(ctx context.Context, query ledger.EntriesQuery) (ledger.EntriesPage, error)
}

// BalanceItem describes one asset balance row (cf-3.1 BalanceItem).
type BalanceItem struct {
	// Currency is the asset code.
	Currency string
	// Available is the available balance.
	Available domain.Money
	// Reserved is the reserved balance.
	Reserved domain.Money
	// Pending is the funds awaiting credit (detected/confirming payments).
	Pending domain.Money
}

// EntriesQuery describes a validated statement request.
type EntriesQuery struct {
	// MerchantID is the token subject (the ownership reference).
	MerchantID string
	// AccountID restricts the statement to one account (empty means all).
	AccountID string
	// From bounds the period from below (inclusive).
	From *time.Time
	// To bounds the period from above (exclusive).
	To *time.Time
	// Limit bounds the page (1..100).
	Limit int
	// Cursor is the opaque ledger cursor.
	Cursor string
}

// StatementService serves the balances and ledger statement endpoints.
type StatementService struct {
	ledger   LedgerClient
	payments PaymentClient
	log      *zap.Logger
	capped   func()
}

// NewStatementService builds the statement service; the capped callback counts
// a pending scan that hit the page cap.
func NewStatementService(ledgerClient LedgerClient, payments PaymentClient, log *zap.Logger, capped func()) *StatementService {
	return &StatementService{ledger: ledgerClient, payments: payments, log: log, capped: capped}
}

// Balances aggregates the per-asset balance rows: available and reserved come
// from the merchant accounts (payout accounts hold funds in flight and are not
// part of the merchant balance), pending from the Payment aggregation.
func (s *StatementService) Balances(ctx context.Context, merchantID string) ([]BalanceItem, error) {
	if s.payments == nil {
		// The pending field is part of the contract response; without Payment
		// the aggregation cannot be complete (FR-7 degradation).
		return nil, ledger.ErrUnavailable
	}
	accounts, err := s.ledger.ListMerchantAccounts(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	items := map[string]*BalanceItem{}
	for _, account := range accounts {
		if account.Type != "merchant" {
			continue
		}
		item, ok := items[account.Asset]
		if !ok {
			// The item is seeded with the asset scale so a zero balance still
			// renders "0.000000" instead of a bare zero.
			item = &BalanceItem{
				Currency:  account.Asset,
				Available: domain.Money{Asset: account.Asset, Decimals: account.Decimals},
				Reserved:  domain.Money{Asset: account.Asset, Decimals: account.Decimals},
			}
			items[account.Asset] = item
		}
		var err error
		item.Available, err = addMoney(item.Available, account.Available)
		if err != nil {
			return nil, err
		}
		item.Reserved, err = addMoney(item.Reserved, account.Reserved)
		if err != nil {
			return nil, err
		}
	}
	for _, state := range []domain.PaymentState{domain.PaymentStateDetected, domain.PaymentStateConfirming} {
		if err := s.accumulatePending(ctx, merchantID, state, items); err != nil {
			return nil, err
		}
	}
	result := make([]BalanceItem, 0, len(items))
	for _, item := range items {
		result = append(result, *item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Currency < result[j].Currency })
	return result, nil
}

// accumulatePending scans the payments of one state and sums the amounts that
// await credit: the received amount when present, otherwise the expected one.
func (s *StatementService) accumulatePending(ctx context.Context, merchantID string, state domain.PaymentState, items map[string]*BalanceItem) error {
	cursor := ""
	for page := 0; page < pendingPageCap; page++ {
		result, err := s.payments.ListPayments(ctx, payment.ListQuery{
			MerchantID:  merchantID,
			StateFilter: state,
			Limit:       pendingPageSize,
			Cursor:      cursor,
		})
		if err != nil {
			return err
		}
		for _, view := range result.Payments {
			amount := view.ReceivedAmount
			if amount == nil || amount.MinorUnits == 0 {
				amount = view.ExpectedAmount
			}
			if amount == nil {
				continue
			}
			item, ok := items[amount.Asset]
			if !ok {
				item = &BalanceItem{
					Currency: amount.Asset,
					Pending:  domain.Money{Asset: amount.Asset, Decimals: amount.Decimals},
				}
				items[amount.Asset] = item
			}
			sum, err := addMoney(item.Pending, *amount)
			if err != nil {
				return err
			}
			item.Pending = sum
		}
		if result.NextCursor == "" {
			return nil
		}
		cursor = result.NextCursor
	}
	s.log.Warn("pending balance scan hit the page cap; the value is approximate",
		zap.String("merchant_id", merchantID), zap.String("state", string(state)))
	if s.capped != nil {
		s.capped()
	}
	return nil
}

// Entries returns the owner-scoped statement page; the Ledger enforces the
// ownership through the merchant reference.
func (s *StatementService) Entries(ctx context.Context, query EntriesQuery) (ledger.EntriesPage, error) {
	return s.ledger.ListEntries(ctx, ledger.EntriesQuery{
		AccountID:   query.AccountID,
		MerchantRef: query.MerchantID,
		PeriodFrom:  query.From,
		PeriodTo:    query.To,
		Limit:       uint32(query.Limit), //nolint:gosec // bounded 1..100 by the transport
		Cursor:      query.Cursor,
	})
}

// addMoney sums two amounts of the same asset and scale; an empty operand
// adopts the other value. An asset or scale mismatch and an int64 overflow are
// errors: a silently distorted balance is worse than a failed read (fail
// closed, security review).
func addMoney(current, delta domain.Money) (domain.Money, error) {
	if delta.MinorUnits == 0 && delta.Asset == "" {
		return current, nil
	}
	if current.MinorUnits == 0 && current.Asset == "" {
		return delta, nil
	}
	if current.Asset != delta.Asset || current.Decimals != delta.Decimals {
		return domain.Money{}, fmt.Errorf("%w: %s/%d and %s/%d", ErrMoneyMismatch,
			current.Asset, current.Decimals, delta.Asset, delta.Decimals)
	}
	sum := current.MinorUnits + delta.MinorUnits
	if (delta.MinorUnits > 0 && sum < current.MinorUnits) || (delta.MinorUnits < 0 && sum > current.MinorUnits) {
		return domain.Money{}, fmt.Errorf("%w: %s", ErrMoneyOverflow, current.Asset)
	}
	return domain.Money{MinorUnits: sum, Asset: current.Asset, Decimals: current.Decimals}, nil
}

// ErrMoneyMismatch reports an asset or scale mismatch in the aggregation.
var ErrMoneyMismatch = errors.New("usecase: balance aggregation asset mismatch")

// ErrMoneyOverflow reports an int64 overflow in the aggregation.
var ErrMoneyOverflow = errors.New("usecase: balance aggregation overflow")
