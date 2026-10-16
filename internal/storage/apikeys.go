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

// Storage errors.
var (
	// ErrNotFound reports a missing record.
	ErrNotFound = errors.New("billing-api storage: not found")
	// ErrDuplicateKey reports a taken unique key (token_hash, prefix).
	ErrDuplicateKey = errors.New("billing-api storage: duplicate key")
	// ErrActiveKeyExists reports an existing active key of the merchant.
	ErrActiveKeyExists = errors.New("billing-api storage: active key already exists")
	// ErrStatusConflict reports that no row matched the concurrent status
	// update (rotation/revocation race).
	ErrStatusConflict = errors.New("billing-api storage: status conflict")
	// ErrSessionUnavailable reports that PostgreSQL is unavailable.
	ErrSessionUnavailable = errors.New("billing-api storage: postgres unavailable")
)

// APIKey describes an api_keys table row.
type APIKey struct {
	// ID defines the record UUIDv7.
	ID string `gorm:"column:id;type:uuid;primaryKey"`
	// MerchantID defines the key owner.
	MerchantID string `gorm:"column:merchant_id"`
	// Prefix defines the public key prefix.
	Prefix string `gorm:"column:prefix"`
	// TokenHash defines the sha256 of the full key.
	TokenHash []byte `gorm:"column:token_hash"`
	// Status defines the lifecycle state.
	Status string `gorm:"column:status"`
	// CreatedAt defines the issue time.
	CreatedAt time.Time `gorm:"column:created_at"`
	// ExpiresAt defines the overlap window end for a retiring key.
	ExpiresAt *time.Time `gorm:"column:expires_at"`
	// RevokedAt defines the revocation time.
	RevokedAt *time.Time `gorm:"column:revoked_at"`
	// LastUsedAt defines the last successful use.
	LastUsedAt *time.Time `gorm:"column:last_used_at"`
	// ExpiresValid defines the DB-computed validity flag (SELECT only).
	ExpiresValid bool `gorm:"column:expires_valid;->"`
}

// TableName defines the keys table name.
func (APIKey) TableName() string { return "api_keys" }

// APIKeyFromDomain converts a domain key record to a table row.
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

// APIKeyToDomain converts a table row to a domain record.
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

// CreateAPIKey inserts a new key record; a uniqueness violation is mapped to
// ErrActiveKeyExists (active key) or ErrDuplicateKey.
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

// FindAPIKeyByHash returns the key by the sha256 of the full token; the
// validity flag (ExpiresValid) is computed by the DB from the server now().
func (r *Repository) FindAPIKeyByHash(ctx context.Context, tokenHash []byte) (domain.APIKey, error) {
	return r.findAPIKey(ctx, "token_hash = ?", tokenHash)
}

// FindAPIKeyByRef returns the key by id or prefix.
func (r *Repository) FindAPIKeyByRef(ctx context.Context, ref string) (domain.APIKey, error) {
	if _, err := uuid.Parse(ref); err == nil {
		return r.findAPIKey(ctx, "id = ?::uuid", ref)
	}
	return r.findAPIKey(ctx, "prefix = ?", ref)
}

// findAPIKey finds the single record matching the condition.
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

// ListAPIKeysByMerchant returns the merchant keys from newest to oldest.
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

// RotateAPIKeys performs the rotation in a single transaction: it revokes the
// existing retiring key, moves active to retiring with a window until
// graceUntil and inserts a new active key. It returns the keys whose
// verification cache entries the usecase logic must update: the revoked
// retiring keys (tombstone) and the active key moved to retiring (DEL).
// A missing active key (race) is ErrStatusConflict.
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

// RevokeAPIKey moves the key to revoked; an already revoked key is returned
// as is (idempotency), a missing one is ErrNotFound.
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
	// A concurrent revoke already applied the terminal state: the second read
	// returns the current record.
	return r.FindAPIKeyByRef(ctx, ref)
}

// TouchAPIKeyLastUsed updates the last used time of the key; revoked keys are
// not changed by the trigger, and the error is not critical for verification.
func (r *Repository) TouchAPIKeyLastUsed(ctx context.Context, id string) error {
	err := r.db.WithContext(ctx).Model(&APIKey{}).
		Where("id = ? AND status <> ?", id, string(domain.StatusRevoked)).
		Update("last_used_at", time.Now().UTC()).Error
	if err != nil {
		return fmt.Errorf("storage: touch api key: %w", err)
	}
	return nil
}

// uniqueViolationConstraint returns the name of the violated uniqueness
// constraint (SQLSTATE 23505) or an empty string.
func uniqueViolationConstraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}
	return ""
}
