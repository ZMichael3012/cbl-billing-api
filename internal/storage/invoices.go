package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cbl/cbl-billing-api/internal/domain"
)

// InvoiceRecord describes an invoices table row (the BFF invoice projection).
type InvoiceRecord struct {
	// PaymentID defines the payment identifier (the invoice id).
	PaymentID string `gorm:"column:payment_id;type:uuid;primaryKey"`
	// MerchantID defines the owning merchant.
	MerchantID string `gorm:"column:merchant_id"`
	// MerchantOrderID defines the merchant order reference.
	MerchantOrderID string `gorm:"column:merchant_order_id"`
	// Metadata defines the merchant metadata JSON object.
	Metadata []byte `gorm:"column:metadata;type:jsonb"`
	// AmountMinor defines the requested amount in minor units.
	AmountMinor int64 `gorm:"column:amount_minor"`
	// AmountAsset defines the requested amount asset.
	AmountAsset string `gorm:"column:amount_asset"`
	// AmountDecimals defines the requested amount scale.
	AmountDecimals uint32 `gorm:"column:amount_decimals"`
	// CreatedAt defines the projection insertion time.
	CreatedAt time.Time `gorm:"column:created_at"`
}

// TableName defines the projection table name.
func (InvoiceRecord) TableName() string { return "invoices" }

// toDomain converts the row into the domain projection.
func (r InvoiceRecord) toDomain() (domain.InvoiceProjection, error) {
	projection := domain.InvoiceProjection{
		PaymentID:       r.PaymentID,
		MerchantID:      r.MerchantID,
		MerchantOrderID: r.MerchantOrderID,
		CreatedAt:       r.CreatedAt,
	}
	if r.AmountAsset != "" {
		projection.Amount = &domain.Money{
			MinorUnits: r.AmountMinor,
			Asset:      r.AmountAsset,
			Decimals:   r.AmountDecimals,
		}
	}
	if len(r.Metadata) > 0 {
		if err := json.Unmarshal(r.Metadata, &projection.Metadata); err != nil {
			return domain.InvoiceProjection{}, fmt.Errorf("storage: decode invoice metadata: %w", err)
		}
	}
	return projection, nil
}

// InsertInvoice inserts the projection; an existing row (a retry after a
// response loss) is kept untouched (idempotent write). The number of inserted
// rows is returned so the caller can detect a foreign collision.
func (r *Repository) InsertInvoice(ctx context.Context, projection domain.InvoiceProjection) (int64, error) {
	metadata := projection.Metadata
	if metadata == nil {
		metadata = map[string]string{}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return 0, fmt.Errorf("storage: encode invoice metadata: %w", err)
	}
	record := InvoiceRecord{
		PaymentID:       projection.PaymentID,
		MerchantID:      projection.MerchantID,
		MerchantOrderID: projection.MerchantOrderID,
		Metadata:        encoded,
		CreatedAt:       projection.CreatedAt.UTC(),
	}
	if projection.Amount != nil {
		record.AmountMinor = projection.Amount.MinorUnits
		record.AmountAsset = projection.Amount.Asset
		record.AmountDecimals = projection.Amount.Decimals
	}
	result := r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&record)
	if result.Error != nil {
		return 0, fmt.Errorf("storage: insert invoice: %w", result.Error)
	}
	return result.RowsAffected, nil
}

// ResolveInvoiceIntent returns the stable expiry anchor of a create intent:
// the stored value when the (merchant, key) intent exists, otherwise the
// proposed value is stored first. The anchor keeps every retry of the derived
// Payment idempotency key byte-identical.
func (r *Repository) ResolveInvoiceIntent(ctx context.Context, merchantID, idempotencyKey string, proposed time.Time) (time.Time, error) {
	intent := InvoiceIntentRecord{
		MerchantID:     merchantID,
		IdempotencyKey: idempotencyKey,
		ExpiresAt:      proposed.UTC(),
		CreatedAt:      time.Now().UTC(),
	}
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&intent).Error
	if err != nil {
		return time.Time{}, fmt.Errorf("storage: insert invoice intent: %w", err)
	}
	var stored InvoiceIntentRecord
	err = r.db.WithContext(ctx).
		Where("merchant_id = ? AND idempotency_key = ?", merchantID, idempotencyKey).
		First(&stored).Error
	if err != nil {
		return time.Time{}, fmt.Errorf("storage: read invoice intent: %w", err)
	}
	return stored.ExpiresAt.UTC(), nil
}

// InvoiceIntentRecord describes an invoice_intents table row.
type InvoiceIntentRecord struct {
	// MerchantID defines the intent owner.
	MerchantID string `gorm:"column:merchant_id;primaryKey"`
	// IdempotencyKey defines the merchant request key.
	IdempotencyKey string `gorm:"column:idempotency_key;primaryKey"`
	// ExpiresAt defines the pinned invoice deadline.
	ExpiresAt time.Time `gorm:"column:expires_at"`
	// CreatedAt defines the intent creation time.
	CreatedAt time.Time `gorm:"column:created_at"`
}

// TableName defines the intent table name.
func (InvoiceIntentRecord) TableName() string { return "invoice_intents" }

// ErrForeign reports a resource that exists but belongs to another merchant;
// the BFF answers 404 without disclosing the existence.
var ErrForeign = errors.New("resource belongs to another merchant")

// GetInvoice returns the projection of one invoice; a foreign owner is
// ErrForeign, a missing row is ErrNotFound.
func (r *Repository) GetInvoice(ctx context.Context, merchantID, paymentID string) (domain.InvoiceProjection, error) {
	var record InvoiceRecord
	err := r.db.WithContext(ctx).
		Where("payment_id = ?", paymentID).
		First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.InvoiceProjection{}, ErrNotFound
	}
	if err != nil {
		return domain.InvoiceProjection{}, fmt.Errorf("storage: get invoice: %w", err)
	}
	if record.MerchantID != merchantID {
		return domain.InvoiceProjection{}, ErrForeign
	}
	return record.toDomain()
}

// ListInvoices returns a projection page ordered by created_at DESC,
// payment_id DESC; the cursor bounds are exclusive.
func (r *Repository) ListInvoices(ctx context.Context, query domain.InvoiceQuery) ([]domain.InvoiceProjection, error) {
	tx := r.db.WithContext(ctx).
		Where("merchant_id = ?", query.MerchantID).
		Order("created_at DESC, payment_id DESC").
		Limit(query.Limit)
	if query.MerchantOrderID != "" {
		tx = tx.Where("merchant_order_id = ?", query.MerchantOrderID)
	}
	if query.AfterCreatedAt != nil {
		tx = tx.Where("(created_at, payment_id) < (?, ?)", *query.AfterCreatedAt, query.AfterPaymentID)
	}
	var records []InvoiceRecord
	if err := tx.Find(&records).Error; err != nil {
		return nil, fmt.Errorf("storage: list invoices: %w", err)
	}
	projections := make([]domain.InvoiceProjection, 0, len(records))
	for _, record := range records {
		projection, err := record.toDomain()
		if err != nil {
			return nil, err
		}
		projections = append(projections, projection)
	}
	return projections, nil
}

// ListInvoicesByIDs returns the projections of the given payment ids owned by
// the merchant (the main list branch hydration).
func (r *Repository) ListInvoicesByIDs(ctx context.Context, merchantID string, paymentIDs []string) (map[string]domain.InvoiceProjection, error) {
	if len(paymentIDs) == 0 {
		return map[string]domain.InvoiceProjection{}, nil
	}
	var records []InvoiceRecord
	err := r.db.WithContext(ctx).
		Where("merchant_id = ? AND payment_id IN ?", merchantID, paymentIDs).
		Find(&records).Error
	if err != nil {
		return nil, fmt.Errorf("storage: list invoices by ids: %w", err)
	}
	projections := make(map[string]domain.InvoiceProjection, len(records))
	for _, record := range records {
		projection, err := record.toDomain()
		if err != nil {
			return nil, err
		}
		projections[record.PaymentID] = projection
	}
	return projections, nil
}
