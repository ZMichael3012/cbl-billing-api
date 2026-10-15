package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// IdempotencyRecord описывает строку таблицы idempotency_keys.
type IdempotencyRecord struct {
	// ID задает UUIDv7 записи.
	ID string `gorm:"column:id;type:uuid;primaryKey"`
	// MerchantID задает владельца ключа.
	MerchantID string `gorm:"column:merchant_id"`
	// Key задает ключ идемпотентности запроса.
	Key string `gorm:"column:key"`
	// RequestHash задает sha256 отпечатка запроса.
	RequestHash []byte `gorm:"column:request_hash"`
	// State задает состояние: processing или completed.
	State string `gorm:"column:state"`
	// ResponseStatus задает статус сохраненного ответа (только completed).
	ResponseStatus *int `gorm:"column:response_status"`
	// ResponseContentType задает Content-Type сохраненного ответа.
	ResponseContentType *string `gorm:"column:response_content_type"`
	// ResponseBody задает тело сохраненного ответа (до предела middleware).
	ResponseBody []byte `gorm:"column:response_body"`
	// CreatedAt задает время вставки записи.
	CreatedAt time.Time `gorm:"column:created_at"`
	// CompletedAt задает время успешного завершения.
	CompletedAt *time.Time `gorm:"column:completed_at"`
}

// TableName задает имя таблицы идемпотентности.
func (IdempotencyRecord) TableName() string { return "idempotency_keys" }

// BeginIdempotency вставляет запись processing; занятый ключ возвращает
// существующую запись (created=false) вместо ошибки - вызывающий решает,
// повтор это, переиспользование или конкурентная обработка.
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

// FindIdempotency возвращает запись по ключу мерчанта.
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

// CompleteIdempotency сохраняет снимок успешного ответа поверх конкретной
// попытки (id записи) с тем же отпечатком: устаревшая попытка не может
// завершить строку нового исполнения (fencing). Отсутствие подходящей строки -
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

// DeleteIdempotencyAttempt удаляет конкретную попытку в ожидаемом состоянии:
// abort обработчика (processing), истечение окна (completed) и брошенная
// обработка (processing). Смена состояния конкурентом - ErrStatusConflict.
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
