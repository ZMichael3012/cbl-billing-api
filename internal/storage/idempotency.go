package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// IdempotencyRecord describes an idempotency_keys table row.
type IdempotencyRecord struct {
	// ID defines the record UUIDv7.
	ID string `gorm:"column:id;type:uuid;primaryKey"`
	// MerchantID defines the key owner.
	MerchantID string `gorm:"column:merchant_id"`
	// Key defines the request idempotency key.
	Key string `gorm:"column:key"`
	// RequestHash defines the sha256 of the request fingerprint.
	RequestHash []byte `gorm:"column:request_hash"`
	// State defines the state: processing or completed.
	State string `gorm:"column:state"`
	// ResponseStatus defines the saved response status (completed only).
	ResponseStatus *int `gorm:"column:response_status"`
	// ResponseContentType defines the saved response Content-Type.
	ResponseContentType *string `gorm:"column:response_content_type"`
	// ResponseBody defines the saved response body (up to the middleware limit).
	ResponseBody []byte `gorm:"column:response_body"`
	// CreatedAt defines the record insertion time.
	CreatedAt time.Time `gorm:"column:created_at"`
	// CompletedAt defines the successful completion time.
	CompletedAt *time.Time `gorm:"column:completed_at"`
}

// TableName defines the idempotency table name.
func (IdempotencyRecord) TableName() string { return "idempotency_keys" }

// BeginIdempotency inserts a processing record; a taken key returns the
// existing record (created=false) instead of an error - the caller decides
// whether this is a retry, a reuse or concurrent processing.
func (r *Repository) BeginIdempotency(ctx context.Context, record IdempotencyRecord) (IdempotencyRecord, bool, error) {
	record.State = "processing"
	record.CreatedAt = time.Now().UTC()
	err := r.db.WithContext(ctx).Create(&record).Error
	if err == nil {
		return record, true, nil
	}
	if uniqueViolationConstraint(err) == "idempotency_keys_merchant_key_uq" {
		existing, findErr := r.FindIdempotency(ctx, record.MerchantID, record.Key)
		if findErr != nil {
			return IdempotencyRecord{}, false, findErr
		}
		return existing, false, nil
	}
	return IdempotencyRecord{}, false, fmt.Errorf("storage: begin idempotency: %w", err)
}

// FindIdempotency returns the record by the merchant key.
func (r *Repository) FindIdempotency(ctx context.Context, merchantID, key string) (IdempotencyRecord, error) {
	var record IdempotencyRecord
	err := r.db.WithContext(ctx).
		Where("merchant_id = ? AND key = ?", merchantID, key).
		First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return IdempotencyRecord{}, ErrNotFound
	}
	if err != nil {
		return IdempotencyRecord{}, fmt.Errorf("storage: find idempotency: %w", err)
	}
	return record, nil
}

// CompleteIdempotency saves the successful response snapshot over a specific
// attempt (record id) with the same fingerprint: a stale attempt cannot
// complete the row of a new execution (fencing). No matching row is
// ErrStatusConflict.
func (r *Repository) CompleteIdempotency(ctx context.Context, recordID string, requestHash []byte, status int, contentType string, body []byte) error {
	now := time.Now().UTC()
	result := r.db.WithContext(ctx).Model(&IdempotencyRecord{}).
		Where("id = ? AND state = ? AND request_hash = ?", recordID, "processing", requestHash).
		Updates(map[string]any{
			"state":                 "completed",
			"response_status":       status,
			"response_content_type": contentType,
			"response_body":         body,
			"completed_at":          now,
		})
	if result.Error != nil {
		return fmt.Errorf("storage: complete idempotency: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("%w: complete attempt %s", ErrStatusConflict, recordID)
	}
	return nil
}

// DeleteIdempotencyAttempt deletes a specific attempt in the expected state:
// a handler abort (processing), window expiry (completed) and an abandoned
// execution (processing). A concurrent state change is ErrStatusConflict.
func (r *Repository) DeleteIdempotencyAttempt(ctx context.Context, recordID, expectedState string) error {
	result := r.db.WithContext(ctx).
		Where("id = ? AND state = ?", recordID, expectedState).
		Delete(&IdempotencyRecord{})
	if result.Error != nil {
		return fmt.Errorf("storage: delete idempotency: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("%w: delete attempt %s", ErrStatusConflict, recordID)
	}
	return nil
}
