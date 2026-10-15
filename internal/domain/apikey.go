package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Параметры формата ключа: cbl_v1_<prefix>_<secret>.
const (
	// tokenScheme задает версионированный префикс ключа: смена схемы
	// выпускается новой версией.
	tokenScheme = "cbl_v1_"
	// PrefixBytes задает длину случайной части prefix в байтах.
	PrefixBytes = 5
	// SecretBytes задает длину секрета ключа в байтах (CSPRNG).
	SecretBytes = 32
)

// tokenPattern задает строгий формат ключа: prefix - 10 hex-символов,
// secret - 43 символа base64url без паддинга.
var tokenPattern = regexp.MustCompile(`^cbl_v1_([0-9a-f]{10})_([A-Za-z0-9_-]{43})$`)

// merchantIDPattern задает форму идентификатора мерчанта: как бизнес-ключ
// плана счетов леджера (CBL-003, раздел 9).
var merchantIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Ошибки домена ключей; транспорт и usecase маппят их в коды ответов.
var (
	// ErrUnauthorized сообщает о неизвестном, отозванном или истекшем ключе.
	ErrUnauthorized = errors.New("apikeys: unauthorized")
	// ErrKeyNotFound сообщает об отсутствии ключа по идентификатору или prefix.
	ErrKeyNotFound = errors.New("apikeys: key not found")
	// ErrKeyAlreadyActive сообщает о существующем активном ключе мерчанта.
	ErrKeyAlreadyActive = errors.New("apikeys: active key already exists")
	// ErrStatusConflict сообщает о конкурентной смене состояния ключа.
	ErrStatusConflict = errors.New("apikeys: key status conflict")
	// ErrInvalidMerchantID сообщает о недопустимом идентификаторе мерчанта.
	ErrInvalidMerchantID = errors.New("apikeys: invalid merchant id")
)

// Status описывает состояние жизненного цикла ключа.
type Status string

// Состояния ключа.
const (
	// StatusActive задает действующий ключ (единственный на мерчанта).
	StatusActive Status = "active"
	// StatusRetiring задает замененный ключ в окне перекрытия ротации.
	StatusRetiring Status = "retiring"
	// StatusRevoked задает отозванный ключ (терминальное состояние).
	StatusRevoked Status = "revoked"
)

// Valid сообщает, что состояние входит в жизненный цикл.
func (s Status) Valid() bool {
	return s == StatusActive || s == StatusRetiring || s == StatusRevoked
}

// APIKey описывает запись ключа; сырой секрет в записи не хранится - только
// TokenHash (sha256 полного ключа).
type APIKey struct {
	// ID задает UUIDv7 записи.
	ID string
	// MerchantID задает владельца ключа.
	MerchantID string
	// Prefix задает публичный префикс ключа для аудита и поиска.
	Prefix string
	// TokenHash задает sha256 полного ключа.
	TokenHash []byte
	// Status задает состояние жизненного цикла.
	Status Status
	// CreatedAt задает время выпуска.
	CreatedAt time.Time
	// ExpiresAt задает конец окна перекрытия для retiring-ключа (7.2).
	ExpiresAt *time.Time
	// RevokedAt задает время отзыва.
	RevokedAt *time.Time
	// LastUsedAt задает последнее успешное использование (best-effort).
	LastUsedAt *time.Time
	// ExpiresValid сообщает, что срок retiring-ключа не истек на момент чтения
	// (вычисляется БД по now() сервера, не часами приложения).
	ExpiresValid bool
}

// ValidMerchantID сообщает, что идентификатор мерчанта допустим.
func ValidMerchantID(merchantID string) bool {
	return merchantIDPattern.MatchString(merchantID)
}

// GenerateToken генерирует новый ключ: возвращает полный токен (показывается
// единственный раз), prefix для записи и sha256-хэш токена для хранения.
func GenerateToken() (token, prefix string, hash []byte, err error) {
	prefixBytes := make([]byte, PrefixBytes)
	if _, err := rand.Read(prefixBytes); err != nil {
		return "", "", nil, fmt.Errorf("apikeys: generate prefix: %w", err)
	}
	secretBytes := make([]byte, SecretBytes)
	if _, err := rand.Read(secretBytes); err != nil {
		return "", "", nil, fmt.Errorf("apikeys: generate secret: %w", err)
	}
	prefix = hex.EncodeToString(prefixBytes)
	token = tokenScheme + prefix + "_" + base64.RawURLEncoding.EncodeToString(secretBytes)
	return token, prefix, HashToken(token), nil
}

// HashToken возвращает sha256 полного токена: единый ключ поиска в БД и в
// Redis-кэше проверки (контракт с периметром, 7.5).
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// ParseToken проверяет формат токена и возвращает его prefix.
func ParseToken(token string) (string, error) {
	matches := tokenPattern.FindStringSubmatch(token)
	if matches == nil {
		return "", fmt.Errorf("%w: token format", ErrUnauthorized)
	}
	return matches[1], nil
}
