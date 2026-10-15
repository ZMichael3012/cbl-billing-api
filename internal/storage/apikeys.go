package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"github.com/cbl/cbl-billing-api/internal/domain"
)

// Ошибки storage.
var (
	// ErrNotFound сообщает об отсутствии записи.
	ErrNotFound = errors.New("billing-api storage: not found")
	// ErrDuplicateKey сообщает о занятом уникальном ключе (token_hash, prefix).
	ErrDuplicateKey = errors.New("billing-api storage: duplicate key")
	// ErrActiveKeyExists сообщает о существующем активном ключе мерчанта.
	ErrActiveKeyExists = errors.New("billing-api storage: active key already exists")
	// ErrStatusConflict сообщает об отсутствии строки под конкурентное
	// обновление статуса (гонка ротации/отзыва).
	ErrStatusConflict = errors.New("billing-api storage: status conflict")
	// ErrSessionUnavailable сообщает о недоступности PostgreSQL.
	ErrSessionUnavailable = errors.New("billing-api storage: postgres unavailable")
)

// APIKey описывает строку таблицы api_keys.
type APIKey struct {
	// ID задает UUIDv7 записи.
	ID string `gorm:"column:id;type:uuid;primaryKey"`
	// MerchantID задает владельца ключа.
	MerchantID string `gorm:"column:merchant_id"`
	// Prefix задает публичный префикс ключа.
	Prefix string `gorm:"column:prefix"`
	// TokenHash задает sha256 полного ключа.
	TokenHash []byte `gorm:"column:token_hash"`
	// Status задает состояние жизненного цикла.
	Status string `gorm:"column:status"`
	// CreatedAt задает время выпуска.
	CreatedAt time.Time `gorm:"column:created_at"`
	// ExpiresAt задает конец окна перекрытия для retiring-ключа.
	ExpiresAt *time.Time `gorm:"column:expires_at"`
	// RevokedAt задает время отзыва.
	RevokedAt *time.Time `gorm:"column:revoked_at"`
	// LastUsedAt задает последнее успешное использование.
	LastUsedAt *time.Time `gorm:"column:last_used_at"`
	// ExpiresValid задает вычисленный БД признак действующего срока (SELECT only).
	ExpiresValid bool `gorm:"column:expires_valid;->"`
}

// TableName задает имя таблицы ключей.
func (APIKey) TableName() string { return "api_keys" }

// APIKeyFromDomain переводит доменную запись ключа в строку таблицы.
func APIKeyFromDomain(key domain.APIKey) APIKey {
	return APIKey{
		ID:         key.ID,
		MerchantID: key.MerchantID,
		Prefix:     key.Prefix,
		TokenHash:  key.TokenHash,
		Status:     string(key.Status),
		CreatedAt:  key.CreatedAt,
		ExpiresAt:  key.ExpiresAt,
		RevokedAt:  key.RevokedAt,
		LastUsedAt: key.LastUsedAt,
	}
}

// APIKeyToDomain переводит строку таблицы в доменную запись.
func APIKeyToDomain(key APIKey) domain.APIKey {
	return domain.APIKey{
		ID:           key.ID,
		MerchantID:   key.MerchantID,
		Prefix:       key.Prefix,
		TokenHash:    key.TokenHash,
		Status:       domain.Status(key.Status),
		CreatedAt:    key.CreatedAt,
		ExpiresAt:    key.ExpiresAt,
		RevokedAt:    key.RevokedAt,
		LastUsedAt:   key.LastUsedAt,
		ExpiresValid: key.ExpiresValid,
	}
}

// CreateAPIKey вставляет новую запись ключа; нарушение уникальности
// отображается в ErrActiveKeyExists (активный ключ) или ErrDuplicateKey.
func (r *Repository) CreateAPIKey(ctx context.Context, key domain.APIKey) error {
	row := APIKeyFromDomain(key)
	err := r.db.WithContext(ctx).Create(&row).Error
	if err == nil {
		return nil
	}
	if constraint := uniqueViolationConstraint(err); constraint != "" {
		if constraint == "api_keys_one_active_idx" {
			return fmt.Errorf("%w: %s", ErrActiveKeyExists, key.MerchantID)
		}
		return fmt.Errorf("%w: constraint %s", ErrDuplicateKey, constraint)
	}
	return fmt.Errorf("storage: create api key: %w", err)
}

// FindAPIKeyByHash возвращает ключ по sha256 полного токена; признак срока
// (ExpiresValid) вычисляется БД по now() сервера.
func (r *Repository) FindAPIKeyByHash(ctx context.Context, tokenHash []byte) (domain.APIKey, error) {
	return r.findAPIKey(ctx, "token_hash = ?", tokenHash)
}

// FindAPIKeyByRef возвращает ключ по id или prefix.
func (r *Repository) FindAPIKeyByRef(ctx context.Context, ref string) (domain.APIKey, error) {
	if _, err := uuid.Parse(ref); err == nil {
		return r.findAPIKey(ctx, "id = ?::uuid", ref)
	}
	return r.findAPIKey(ctx, "prefix = ?", ref)
}

// findAPIKey ищет единственную запись по условию.
func (r *Repository) findAPIKey(ctx context.Context, where string, args ...any) (domain.APIKey, error) {
	var row APIKey
	err := r.db.WithContext(ctx).
		Select("api_keys.*, (api_keys.status = 'active' OR api_keys.expires_at IS NULL OR api_keys.expires_at > now()) AS expires_valid").
		Where(where, args...).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.APIKey{}, ErrNotFound
	}
	if err != nil {
		return domain.APIKey{}, fmt.Errorf("storage: find api key: %w", err)
	}
	return APIKeyToDomain(row), nil
}

// ListAPIKeysByMerchant возвращает ключи мерчанта от новых к старым.
func (r *Repository) ListAPIKeysByMerchant(ctx context.Context, merchantID string) ([]domain.APIKey, error) {
	var rows []APIKey
	err := r.db.WithContext(ctx).
		Where("merchant_id = ?", merchantID).
		Order("created_at DESC, id DESC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("storage: list api keys: %w", err)
	}
	keys := make([]domain.APIKey, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, APIKeyToDomain(row))
	}
	return keys, nil
}

// RotateAPIKeys выполняет ротацию в одной транзакции: отзывает существующий
// retiring-ключ, переводит active в retiring с окном до graceUntil и вставляет
// новый active. Возвращает ключи, чьи записи кэша проверки обязана обновить
// usecase-логика: отозванные retiring (tombstone) и переведенный в retiring
// active (DEL). Отсутствие active-ключа (гонка) - ErrStatusConflict.
func (r *Repository) RotateAPIKeys(ctx context.Context, merchantID string, newKey domain.APIKey, graceUntil time.Time) ([]domain.APIKey, domain.APIKey, error) {
	var revokedRetiring []domain.APIKey
	var retiredActive domain.APIKey
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		var retiring []APIKey
		if err := tx.Where("merchant_id = ? AND status = ?", merchantID, string(domain.StatusRetiring)).
			Find(&retiring).Error; err != nil {
			return fmt.Errorf("storage: load retiring key: %w", err)
		}
		for _, row := range retiring {
			result := tx.Model(&APIKey{}).Where("id = ? AND status = ?", row.ID, string(domain.StatusRetiring)).
				Updates(map[string]any{"status": string(domain.StatusRevoked), "revoked_at": now})
			if result.Error != nil {
				return fmt.Errorf("storage: revoke retiring key: %w", result.Error)
			}
			if result.RowsAffected != 1 {
				return fmt.Errorf("%w: retiring key %s", ErrStatusConflict, row.Prefix)
			}
			revokedRetiring = append(revokedRetiring, APIKeyToDomain(row))
		}

		var active APIKey
		if err := tx.Where("merchant_id = ? AND status = ?", merchantID, string(domain.StatusActive)).
			First(&active).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: active key for %s", ErrStatusConflict, merchantID)
			}
			return fmt.Errorf("storage: load active key: %w", err)
		}
		result := tx.Model(&APIKey{}).Where("id = ? AND status = ?", active.ID, string(domain.StatusActive)).
			Updates(map[string]any{"status": string(domain.StatusRetiring), "expires_at": graceUntil.UTC()})
		if result.Error != nil {
			return fmt.Errorf("storage: retire active key: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: active key %s", ErrStatusConflict, active.Prefix)
		}
		retiredActive = APIKeyToDomain(active)

		row := APIKeyFromDomain(newKey)
		if err := tx.Create(&row).Error; err != nil {
			if constraint := uniqueViolationConstraint(err); constraint != "" {
				return fmt.Errorf("%w: constraint %s", ErrDuplicateKey, constraint)
			}
			return fmt.Errorf("storage: create rotated api key: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, domain.APIKey{}, err
	}
	return revokedRetiring, retiredActive, nil
}

// RevokeAPIKey переводит ключ в revoked; уже отозванный ключ возвращается как
// есть (идемпотентность), отсутствие - ErrNotFound.
func (r *Repository) RevokeAPIKey(ctx context.Context, ref string) (domain.APIKey, error) {
	key, err := r.FindAPIKeyByRef(ctx, ref)
	if err != nil {
		return domain.APIKey{}, err
	}
	if key.Status == domain.StatusRevoked {
		return key, nil
	}
	now := time.Now().UTC()
	result := r.db.WithContext(ctx).Model(&APIKey{}).
		Where("id = ? AND status <> ?", key.ID, string(domain.StatusRevoked)).
		Updates(map[string]any{"status": string(domain.StatusRevoked), "revoked_at": now})
	if result.Error != nil {
		return domain.APIKey{}, fmt.Errorf("storage: revoke api key: %w", result.Error)
	}
	// Конкурентный отзыв уже применил терминальное состояние: повторное чтение
	// возвращает актуальную запись.
	return r.FindAPIKeyByRef(ctx, ref)
}

// TouchAPIKeyLastUsed обновляет время последнего использования ключа;
// отозванные ключи триггером не изменяются, ошибка не критична для проверки.
func (r *Repository) TouchAPIKeyLastUsed(ctx context.Context, id string) error {
	err := r.db.WithContext(ctx).Model(&APIKey{}).
		Where("id = ? AND status <> ?", id, string(domain.StatusRevoked)).
		Update("last_used_at", time.Now().UTC()).Error
	if err != nil {
		return fmt.Errorf("storage: touch api key: %w", err)
	}
	return nil
}

// uniqueViolationConstraint возвращает имя нарушенного ограничения
// уникальности (SQLSTATE 23505) или пустую строку.
func uniqueViolationConstraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}
	return ""
}
