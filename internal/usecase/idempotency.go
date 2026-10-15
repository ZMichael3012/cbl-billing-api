package usecase

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/storage"
)

// Константы кэша идемпотентности: ключ Redis - контракт CBL-003, раздел 7.
const (
	// idempotencyCachePrefix задает префикс ключей кэша ответов.
	idempotencyCachePrefix = "idem:v1:"
	// MaxIdempotencyResponseBytes задает предел тела сохраняемого ответа;
	// больший ответ не кэшируется (повтор выполнится заново).
	MaxIdempotencyResponseBytes = 256 * 1024
	// maxRebeginDepth ограничивает повторные попытки переиспользования записи
	// при конкурентных гонках (delete+insert) и защищает от циклов.
	maxRebeginDepth = 2
)

// Границы конфигурации идемпотентности.
const (
	// minIdempotencyTTL задает минимальное окно хранения.
	minIdempotencyTTL = time.Hour
	// maxIdempotencyTTL задает максимальное окно хранения.
	maxIdempotencyTTL = 7 * 24 * time.Hour
	// minProcessingTimeout задает минимальный таймаут брошенной обработки.
	minProcessingTimeout = 30 * time.Second
	// maxProcessingTimeout задает максимальный таймаут брошенной обработки.
	maxProcessingTimeout = 15 * time.Minute
)

// Ошибки конфигурации сервиса идемпотентности.
var (
	// ErrIdempotencyStoreRequired сообщает об отсутствии хранилища.
	ErrIdempotencyStoreRequired = errors.New("usecase: idempotency store is required")
	// ErrIdempotencyConfig сообщает о недопустимой конфигурации.
	ErrIdempotencyConfig = errors.New("usecase: invalid idempotency config")
)

// IdempotencyStore описывает хранилище записей идемпотентности; попытки
// адресуются идентификатором записи (fencing: устаревший Finish не может
// завершить или удалить строку нового исполнения).
type IdempotencyStore interface {
	BeginIdempotency(ctx context.Context, record storage.IdempotencyRecord) (storage.IdempotencyRecord, bool, error)
	CompleteIdempotency(ctx context.Context, recordID string, requestHash []byte, status int, contentType string, body []byte) error
	DeleteIdempotencyAttempt(ctx context.Context, recordID, expectedState string) error
}

// Snapshot описывает сохраненный ответ для повтора.
type Snapshot struct {
	// Status задает HTTP-статус исходного ответа.
	Status int
	// ContentType задает Content-Type исходного ответа.
	ContentType string
	// Body задает тело исходного ответа.
	Body []byte
}

// DecisionKind задает решение по запросу с ключом идемпотентности.
type DecisionKind int

// Решения по запросу.
const (
	// DecisionProceed означает выполнение обработчика.
	DecisionProceed DecisionKind = iota
	// DecisionReplay означает выдачу сохраненного ответа.
	DecisionReplay
	// DecisionConflict означает отказ (reuse ключа или обработка in progress).
	DecisionConflict
)

// Decision описывает результат Begin.
type Decision struct {
	// Kind задает вид решения.
	Kind DecisionKind
	// AttemptID задает идентификатор записи-попытки при DecisionProceed;
	// Finish обязан адресовать именно ее.
	AttemptID string
	// Replay задает сохраненный ответ при DecisionReplay.
	Replay Snapshot
	// Reason задает причину отказа при DecisionConflict.
	Reason error
}

// IdempotencyConfig задает параметры сервиса идемпотентности.
type IdempotencyConfig struct {
	// TTL задает окно хранения ответов.
	TTL time.Duration
	// ProcessingTimeout задает таймаут брошенной обработки.
	ProcessingTimeout time.Duration
}

// Idempotency реализует решения идемпотентности мерчант API (7.3):
// быстрый путь Redis и источник истины PostgreSQL.
type Idempotency struct {
	store IdempotencyStore
	cache KeyCache
	log   *zap.Logger
	cfg   IdempotencyConfig
	now   func() time.Time
}

// cachedIdempotency описывает значение кэша ответов.
type cachedIdempotency struct {
	// Hash задает hex отпечатка запроса.
	Hash string `json:"hash"`
	// Status задает HTTP-статус ответа.
	Status int `json:"status"`
	// ContentType задает Content-Type ответа.
	ContentType string `json:"content_type"`
	// BodyBase64 задает тело ответа в base64.
	BodyBase64 string `json:"body_base64"`
	// CompletedAt задает время завершения исходного запроса (окно TTL).
	CompletedAt time.Time `json:"completed_at"`
}

// NewIdempotency создает сервис идемпотентности; конфигурация валидируется
// fail-fast, nil-кэш означает работу без Redis.
func NewIdempotency(store IdempotencyStore, cache KeyCache, log *zap.Logger, cfg IdempotencyConfig) (*Idempotency, error) {
	if store == nil {
		return nil, ErrIdempotencyStoreRequired
	}
	if cfg.TTL < minIdempotencyTTL || cfg.TTL > maxIdempotencyTTL {
		return nil, fmt.Errorf("%w: ttl %s outside %s..%s", ErrIdempotencyConfig, cfg.TTL, minIdempotencyTTL, maxIdempotencyTTL)
	}
	if cfg.ProcessingTimeout < minProcessingTimeout || cfg.ProcessingTimeout > maxProcessingTimeout {
		return nil, fmt.Errorf("%w: processing timeout %s outside %s..%s",
			ErrIdempotencyConfig, cfg.ProcessingTimeout, minProcessingTimeout, maxProcessingTimeout)
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Idempotency{store: store, cache: cache, log: log, cfg: cfg, now: time.Now}, nil
}

// Begin принимает решение по запросу: выполнить, повторить ответ или отказать.
// Значение кэша проверяется на целостность и окно: подмена, порча или
// истечение дают промах и решение по PostgreSQL (источник истины).
func (i *Idempotency) Begin(ctx context.Context, merchantID, key string, fingerprint []byte) (Decision, error) {
	cacheKey := idempotencyCacheKey(merchantID, key)
	hashHex := fmt.Sprintf("%x", fingerprint)

	if cached, found, err := i.cacheGet(ctx, cacheKey); err != nil {
		i.log.Warn("idempotency cache read failed", zap.Error(err))
	} else if found {
		if cached.expired(i.now().UTC(), i.cfg.TTL) {
			i.cacheDelete(ctx, cacheKey)
		} else if cached.Hash == hashHex {
			return Decision{Kind: DecisionReplay, Replay: cached.Snapshot()}, nil
		}
		// Несовпадающий хэш или испорченное значение решает PostgreSQL:
		// оракул 409 по кэшу не допускается.
	}

	attemptID, err := newIdempotencyID()
	if err != nil {
		return Decision{}, err
	}
	record, created, err := i.store.BeginIdempotency(ctx, storage.IdempotencyRecord{
		ID:          attemptID,
		MerchantID:  merchantID,
		Key:         key,
		RequestHash: fingerprint,
	})
	if err != nil {
		return Decision{}, err
	}
	if created {
		return Decision{Kind: DecisionProceed, AttemptID: record.ID}, nil
	}
	return i.decideExisting(ctx, cacheKey, record, fingerprint, hashHex, 0)
}

// decideExisting разбирает уже существующую запись: повтор, переиспользование,
// конкурентную обработку, истечение окна или брошенную обработку.
func (i *Idempotency) decideExisting(ctx context.Context, cacheKey string, record storage.IdempotencyRecord, fingerprint []byte, hashHex string, depth int) (Decision, error) {
	if !bytes.Equal(record.RequestHash, fingerprint) {
		// Отпечаток другого тела: переиспользование ключа. Конфликт живет,
		// пока строка не удалена очисткой 14.2 (осознанно: контракт не
		// обещает освобождение ключа после окна).
		return Decision{Kind: DecisionConflict, Reason: domain.ErrIdempotencyKeyReuse}, nil
	}
	now := i.now().UTC()
	switch record.State {
	case "completed":
		if record.CompletedAt != nil && now.Sub(*record.CompletedAt) >= i.cfg.TTL {
			return i.rebegin(ctx, cacheKey, record, "completed", fingerprint, hashHex, depth)
		}
		snapshot := Snapshot{
			Status:      derefInt(record.ResponseStatus),
			ContentType: derefString(record.ResponseContentType),
			Body:        record.ResponseBody,
		}
		if record.CompletedAt != nil {
			i.cacheSet(ctx, cacheKey, snapshot, hashHex, *record.CompletedAt)
		}
		return Decision{Kind: DecisionReplay, Replay: snapshot}, nil
	default:
		if now.Sub(record.CreatedAt) >= i.cfg.ProcessingTimeout {
			return i.rebegin(ctx, cacheKey, record, "processing", fingerprint, hashHex, depth)
		}
		return Decision{Kind: DecisionConflict, Reason: domain.ErrIdempotencyInProgress}, nil
	}
}

// rebegin переиспользует истекшую или брошенную запись: удаляет конкретную
// попытку (fencing по id и состоянию) и вставляет новую processing.
func (i *Idempotency) rebegin(ctx context.Context, cacheKey string, record storage.IdempotencyRecord, expectedState string, fingerprint []byte, hashHex string, depth int) (Decision, error) {
	if depth >= maxRebeginDepth {
		return Decision{Kind: DecisionConflict, Reason: domain.ErrIdempotencyInProgress}, nil
	}
	if err := i.store.DeleteIdempotencyAttempt(ctx, record.ID, expectedState); err != nil {
		return Decision{}, err
	}
	i.cacheDelete(ctx, cacheKey)
	attemptID, err := newIdempotencyID()
	if err != nil {
		return Decision{}, err
	}
	fresh, created, err := i.store.BeginIdempotency(ctx, storage.IdempotencyRecord{
		ID:          attemptID,
		MerchantID:  record.MerchantID,
		Key:         record.Key,
		RequestHash: fingerprint,
	})
	if err != nil {
		return Decision{}, err
	}
	if created {
		return Decision{Kind: DecisionProceed, AttemptID: fresh.ID}, nil
	}
	return i.decideExisting(ctx, cacheKey, fresh, fingerprint, hashHex, depth+1)
}

// Finish фиксирует исход обработчика адресованной попытки: успех сохраняется
// снимком (PG и Redis), ошибка освобождает ключ; слишком большой ответ
// (overflow) не кэшируется. Устаревшая попытка не может затронуть строку
// нового исполнения (fencing по id).
func (i *Idempotency) Finish(ctx context.Context, merchantID, key, attemptID string, fingerprint []byte, status int, contentType string, body []byte, overflow bool) error {
	if overflow || status < 200 || status >= 300 {
		if overflow {
			i.log.Warn("idempotency response too large, snapshot skipped", zap.String("merchant_id", merchantID))
		}
		if err := i.store.DeleteIdempotencyAttempt(ctx, attemptID, "processing"); err != nil {
			// Конкурент уже сменил состояние попытки (таймаут): освобождать
			// нечего, ключ займет новое исполнение.
			i.log.Warn("idempotency abort skipped", zap.String("merchant_id", merchantID), zap.Error(err))
		}
		return nil
	}
	if err := i.store.CompleteIdempotency(ctx, attemptID, fingerprint, status, contentType, body); err != nil {
		// Ответ уже вычислен: фиксируем в Redis best-effort; строка processing
		// истечет по таймауту, повтор запроса переобработается.
		i.log.Warn("idempotency completion failed", zap.String("merchant_id", merchantID), zap.Error(err))
	}
	completedAt := i.now().UTC()
	i.cacheSet(ctx, idempotencyCacheKey(merchantID, key), Snapshot{
		Status:      status,
		ContentType: contentType,
		Body:        body,
	}, fmt.Sprintf("%x", fingerprint), completedAt)
	return nil
}

// cacheGet читает кэш и валидирует значение: порча, недопустимый статус или
// хэш дают промах (решение принимает PostgreSQL).
func (i *Idempotency) cacheGet(ctx context.Context, cacheKey string) (cachedIdempotency, bool, error) {
	if i.cache == nil {
		return cachedIdempotency{}, false, nil
	}
	raw, found, err := i.cache.Get(ctx, cacheKey)
	if err != nil || !found {
		return cachedIdempotency{}, false, err
	}
	var cached cachedIdempotency
	if unmarshalErr := json.Unmarshal([]byte(raw), &cached); unmarshalErr != nil {
		i.log.Warn("idempotency cache value is malformed", zap.Error(unmarshalErr))
		return cachedIdempotency{}, false, nil
	}
	if !cached.valid() {
		i.log.Warn("idempotency cache value is invalid")
		return cachedIdempotency{}, false, nil
	}
	return cached, true, nil
}

// cacheSet пишет снимок в кэш с остатком окна TTL от completedAt
// (best-effort); истекшее окно в кэш не пишется.
func (i *Idempotency) cacheSet(ctx context.Context, cacheKey string, snapshot Snapshot, hashHex string, completedAt time.Time) {
	if i.cache == nil {
		return
	}
	remaining := i.cfg.TTL - i.now().UTC().Sub(completedAt)
	if remaining <= 0 {
		return
	}
	payload, err := json.Marshal(cachedIdempotency{
		Hash:        hashHex,
		Status:      snapshot.Status,
		ContentType: snapshot.ContentType,
		BodyBase64:  base64.StdEncoding.EncodeToString(snapshot.Body),
		CompletedAt: completedAt,
	})
	if err != nil {
		i.log.Warn("idempotency cache encode failed", zap.Error(err))
		return
	}
	if err := i.cache.Set(ctx, cacheKey, string(payload), remaining); err != nil {
		i.log.Warn("idempotency cache write failed", zap.Error(err))
	}
}

// cacheDelete удаляет значение кэша best-effort.
func (i *Idempotency) cacheDelete(ctx context.Context, cacheKey string) {
	if i.cache == nil {
		return
	}
	if err := i.cache.Del(ctx, cacheKey); err != nil {
		i.log.Warn("idempotency cache delete failed", zap.Error(err))
	}
}

// valid проверяет целостность значения кэша: хэш-форма, статус 2xx и
// декодируемое тело.
func (c cachedIdempotency) valid() bool {
	if len(c.Hash) != 64 {
		return false
	}
	if _, err := hex.DecodeString(c.Hash); err != nil {
		return false
	}
	if c.Status < 200 || c.Status >= 300 {
		return false
	}
	if !c.CompletedAt.After(time.Time{}) {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(c.BodyBase64)
	return err == nil
}

// expired сообщает, что окно хранения значения кэша истекло.
func (c cachedIdempotency) expired(now time.Time, ttl time.Duration) bool {
	return now.Sub(c.CompletedAt) >= ttl
}

// Snapshot восстанавливает снимок ответа из значения кэша.
func (c cachedIdempotency) Snapshot() Snapshot {
	body, _ := base64.StdEncoding.DecodeString(c.BodyBase64)
	return Snapshot{Status: c.Status, ContentType: c.ContentType, Body: body}
}

// newIdempotencyID генерирует идентификатор попытки.
func newIdempotencyID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("usecase: generate idempotency id: %w", err)
	}
	return id.String(), nil
}

// idempotencyCacheKey собирает ключ кэша ответов.
func idempotencyCacheKey(merchantID, key string) string {
	return idempotencyCachePrefix + merchantID + ":" + key
}

// derefInt разыменовывает необязательное целое.
func derefInt(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

// derefString разыменовывает необязательную строку.
func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
