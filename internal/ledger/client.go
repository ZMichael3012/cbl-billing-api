// Package ledger wraps the LedgerService gRPC client for the merchant API
// balances and statement endpoints (CBL-17.14).
package ledger

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/cbl/cbl-protos/gen/go/billing/common/v1"
	ledgerv1 "github.com/cbl/cbl-protos/gen/go/billing/ledger/v1"

	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/grpcmoney"
)

// Proxy domain errors (the transport translates them into problem+json).
var (
	// ErrUnavailable means the Ledger is unavailable or timed out.
	ErrUnavailable = errors.New("ledger unavailable")
	// ErrRejected means the Ledger rejected the query (422 class).
	ErrRejected = errors.New("ledger rejected the query")
	// ErrInvalid means a malformed query was rejected (400 class).
	ErrInvalid = errors.New("ledger query invalid")
	// ErrNotFound means the account is unknown or belongs to another merchant.
	ErrNotFound = errors.New("ledger account not found")
)

// MerchantAccount describes one merchant account with its balances.
type MerchantAccount struct {
	// AccountID is the deterministic account identifier.
	AccountID string
	// Type is the chart-of-accounts type: merchant or payout.
	Type string
	// Asset is the account asset.
	Asset string
	// Decimals is the asset scale.
	Decimals uint32
	// Available is the available balance.
	Available domain.Money
	// Reserved is the reserved balance.
	Reserved domain.Money
}

// EntriesQuery describes a statement call; AccountID or MerchantRef is set.
type EntriesQuery struct {
	// AccountID restricts the statement to one account.
	AccountID string
	// MerchantRef restricts the statement to the merchant accounts (ownership).
	MerchantRef string
	// PeriodFrom bounds the period from below (inclusive).
	PeriodFrom *time.Time
	// PeriodTo bounds the period from above (exclusive).
	PeriodTo *time.Time
	// Limit bounds the page.
	Limit uint32
	// Cursor is the opaque ledger cursor.
	Cursor string
}

// Entry describes one ledger entry.
type Entry struct {
	// EntryID is the monotonic entry identifier.
	EntryID int64
	// AccountID is the account the entry belongs to.
	AccountID string
	// Direction is the entry direction: debit or credit.
	Direction string
	// Amount is the entry amount.
	Amount domain.Money
	// RefType is the source operation type.
	RefType string
	// RefID is the source operation identifier.
	RefID string
	// CreatedAt is the posting time.
	CreatedAt time.Time
}

// EntriesPage describes a statement page.
type EntriesPage struct {
	// Entries sets the page rows.
	Entries []Entry
	// NextCursor sets the next page cursor.
	NextCursor string
}

// Client wraps the LedgerService gRPC client with a shared call timeout.
type Client struct {
	conn    *grpc.ClientConn
	svc     ledgerv1.LedgerServiceClient
	timeout time.Duration
}

// NewClient connects to LedgerService at host:port (plaintext inside the
// trusted network; mTLS comes in 0.6).
func NewClient(addr string, timeout time.Duration) (*Client, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("ledger addr %q: %w", addr, err)
	}
	if timeout <= 0 {
		return nil, errors.New("ledger timeout must be positive")
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial ledger: %w", err)
	}
	return &Client{conn: conn, svc: ledgerv1.NewLedgerServiceClient(conn), timeout: timeout}, nil
}

// Close closes the gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// ListMerchantAccounts returns the merchant accounts with balances; the
// Ledger enforces the merchant reference ownership.
func (c *Client) ListMerchantAccounts(ctx context.Context, merchantRef string) ([]MerchantAccount, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	response, err := c.svc.ListMerchantAccounts(callCtx, &ledgerv1.ListMerchantAccountsRequest{
		MerchantRef: merchantRef,
	})
	if err != nil {
		return nil, classify(err)
	}
	accounts := make([]MerchantAccount, 0, len(response.GetAccounts()))
	for _, account := range response.GetAccounts() {
		item := MerchantAccount{
			AccountID: account.GetAccountId(),
			Type:      account.GetType(),
			Asset:     account.GetAsset(),
			Decimals:  account.GetDecimals(),
		}
		if available := grpcmoney.FromProto(account.GetAvailable()); available != nil {
			item.Available = *available
		}
		if reserved := grpcmoney.FromProto(account.GetReserved()); reserved != nil {
			item.Reserved = *reserved
		}
		accounts = append(accounts, item)
	}
	return accounts, nil
}

// ListEntries returns a statement page; the Ledger enforces the ownership when
// MerchantRef is set (an account outside the merchant set is not found).
func (c *Client) ListEntries(ctx context.Context, query EntriesQuery) (EntriesPage, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request := &ledgerv1.ListEntriesRequest{
		AccountId:   query.AccountID,
		MerchantRef: query.MerchantRef,
		Limit:       query.Limit,
		Cursor:      query.Cursor,
	}
	if query.PeriodFrom != nil {
		request.PeriodFrom = timestamppb.New(*query.PeriodFrom)
	}
	if query.PeriodTo != nil {
		request.PeriodTo = timestamppb.New(*query.PeriodTo)
	}
	response, err := c.svc.ListEntries(callCtx, request)
	if err != nil {
		return EntriesPage{}, classify(err)
	}
	entries := make([]Entry, 0, len(response.GetEntries()))
	for _, entry := range response.GetEntries() {
		item := Entry{
			EntryID:   entry.GetEntryId(),
			AccountID: entry.GetAccountId(),
			Direction: directionWord(entry.GetDirection()),
			RefType:   entry.GetRefType(),
			RefID:     entry.GetRefId(),
		}
		if amount := grpcmoney.FromProto(entry.GetAmount()); amount != nil {
			item.Amount = *amount
		}
		if ts := entry.GetCreatedAt(); ts != nil {
			item.CreatedAt = ts.AsTime().UTC()
		}
		entries = append(entries, item)
	}
	return EntriesPage{Entries: entries, NextCursor: response.GetNextCursor()}, nil
}

// classify translates a gRPC status into proxy domain errors.
func classify(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	switch st.Code() {
	case codes.InvalidArgument:
		return fmt.Errorf("%w: %s", ErrInvalid, st.Message())
	case codes.NotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, st.Message())
	default:
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
}

// directionWord maps the contract direction enum into the REST word.
func directionWord(direction commonv1.Direction) string {
	switch direction {
	case commonv1.Direction_DIRECTION_DEBIT:
		return "debit"
	case commonv1.Direction_DIRECTION_CREDIT:
		return "credit"
	default:
		return strings.ToLower(strings.TrimPrefix(direction.String(), "DIRECTION_"))
	}
}
