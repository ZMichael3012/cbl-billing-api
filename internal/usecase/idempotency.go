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

// Idempotency cache constants: the Redis key is the CBL-003 contract, section 7.
const (
	// idempotencyCachePrefix sets the prefix of response cache keys.
	idempotencyCachePrefix = "idem:v1:"
	// MaxIdempotencyResponseBytes sets the saved response body limit; a larger
	// response is not cached (a retry will be executed again).
	MaxIdempotencyResponseBytes = 256 * 1024
	// maxRebeginDepth limits repeated attempts to reuse a record under
	// concurrent races (delete+insert) and protects against loops.
	maxRebeginDepth = 2
)

// Idempotency configuration boundaries.
const (
	// minIdempotencyTTL sets the minimum retention window.
	minIdempotencyTTL = time.Hour
	// maxIdempotencyTTL sets the maximum retention window.
	maxIdempotencyTTL = 7 * 24 * time.Hour
	// minProcessingTimeout sets the minimum abandoned processing timeout.
	minProcessingTimeout = 30 * time.Second
	// maxProcessingTimeout sets the maximum abandoned processing timeout.
	maxProcessingTimeout = 15 * time.Minute
)

// Idempotency service configuration errors.
var (
	// ErrIdempotencyStoreRequired reports a missing storage.
	ErrIdempotencyStoreRequired = errors.New("usecase: idempotency store is required")
	// ErrIdempotencyConfig reports an invalid configuration.
	ErrIdempotencyConfig = errors.New("usecase: invalid idempotency config")
)

// IdempotencyStore describes the idempotency record storage; attempts are
// addressed by record id (fencing: a stale Finish cannot complete or delete the
// record of a new execution).
type IdempotencyStore interface {
	BeginIdempotency(ctx context.Context, record storage.IdempotencyRecord) (storage.IdempotencyRecord, bool, error)
	CompleteIdempotency(ctx context.Context, recordID string, requestHash []byte, status int, contentType string, body []byte) error
	DeleteIdempotencyAttempt(ctx context.Context, recordID, expectedState string) error
}

// Snapshot describes a saved response for a retry.
type Snapshot struct {
	// Status sets the HTTP status of the original response.
	Status int
	// ContentType sets the Content-Type of the original response.
	ContentType string
	// Body sets the body of the original response.
	Body []byte
}

// DecisionKind sets the decision for a request with an idempotency key.
type DecisionKind int

// Decisions for a request.
const (
	// DecisionProceed means the handler is executed.
	DecisionProceed DecisionKind = iota
	// DecisionReplay means the saved response is returned.
	DecisionReplay
	// DecisionConflict means rejection (key reuse or processing in progress).
	DecisionConflict
)

// Decision describes the result of Begin.
type Decision struct {
	// Kind sets the decision kind.
	Kind DecisionKind
	// AttemptID sets the attempt record id under DecisionProceed; Finish is
	// required to address exactly it.
	AttemptID string
	// Replay sets the saved response under DecisionReplay.
	Replay Snapshot
	// Reason sets the rejection reason under DecisionConflict.
	Reason error
}

// IdempotencyConfig sets the idempotency service parameters.
type IdempotencyConfig struct {
	// TTL sets the response retention window.
	TTL time.Duration
	// ProcessingTimeout sets the abandoned processing timeout.
	ProcessingTimeout time.Duration
}

// Idempotency implements merchant API idempotency decisions (7.3): the Redis
// fast path and the PostgreSQL source of truth.
type Idempotency struct {
	store IdempotencyStore
	cache KeyCache
	log   *zap.Logger
	cfg   IdempotencyConfig
	now   func() time.Time
}

// cachedIdempotency describes the response cache value.
type cachedIdempotency struct {
	// Hash sets the hex fingerprint of the request.
	Hash string `json:"hash"`
	// Status sets the HTTP status of the response.
	Status int `json:"status"`
	// ContentType sets the Content-Type of the response.
	ContentType string `json:"content_type"`
	// BodyBase64 sets the response body in base64.
	BodyBase64 string `json:"body_base64"`
	// CompletedAt sets the completion time of the original request (the TTL window).
	CompletedAt time.Time `json:"completed_at"`
}

// NewIdempotency creates the idempotency service; the configuration is
// validated fail-fast, a nil cache means operation without Redis.
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

// Begin makes a decision for a request: proceed, replay the response or reject.
// The cache value is checked for integrity and window: a substitution,
// corruption or expiration causes a miss and a PostgreSQL decision (source of truth).
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
		// A mismatching hash or a corrupted value is decided by PostgreSQL:
		// a 409 oracle based on the cache is not allowed.
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

// decideExisting handles an existing record: replay, reuse, concurrent
// processing, window expiration or abandoned processing.
func (i *Idempotency) decideExisting(ctx context.Context, cacheKey string, record storage.IdempotencyRecord, fingerprint []byte, hashHex string, depth int) (Decision, error) {
	if !bytes.Equal(record.RequestHash, fingerprint) {
		// The fingerprint of a different body: key reuse. The conflict lives
		// until the record is deleted by cleanup 14.2 (deliberately: the
		// contract does not promise key release after the window).
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

// rebegin reuses an expired or abandoned record: it deletes the specific
// attempt (fencing by id and state) and inserts a new processing one.
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

// Finish commits the handler outcome of the addressed attempt: a success is
// saved as a snapshot (PG and Redis), an error releases the key; a too large
// response (overflow) is not cached. A stale attempt cannot affect the record
// of a new execution (fencing by id).
func (i *Idempotency) Finish(ctx context.Context, merchantID, key, attemptID string, fingerprint []byte, status int, contentType string, body []byte, overflow bool) error {
	if overflow || status < 200 || status >= 300 {
		if overflow {
			i.log.Warn("idempotency response too large, snapshot skipped", zap.String("merchant_id", merchantID))
		}
		if err := i.store.DeleteIdempotencyAttempt(ctx, attemptID, "processing"); err != nil {
			// A concurrent request has already changed the attempt state (timeout):
			// there is nothing to release, a new execution will take the key.
			i.log.Warn("idempotency abort skipped", zap.String("merchant_id", merchantID), zap.Error(err))
		}
		return nil
	}
	if err := i.store.CompleteIdempotency(ctx, attemptID, fingerprint, status, contentType, body); err != nil {
		// The response is already computed: we save it to Redis best-effort; the
		// processing record will expire by timeout, the request retry will be
		// reprocessed.
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

// cacheGet reads the cache and validates the value: corruption, an invalid
// status or hash cause a miss (PostgreSQL makes the decision).
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

// cacheSet writes a snapshot to the cache with the remaining TTL window from
// completedAt (best-effort); an expired window is not written to the cache.
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

// cacheDelete deletes the cache value best-effort.
func (i *Idempotency) cacheDelete(ctx context.Context, cacheKey string) {
	if i.cache == nil {
		return
	}
	if err := i.cache.Del(ctx, cacheKey); err != nil {
		i.log.Warn("idempotency cache delete failed", zap.Error(err))
	}
}

// valid checks the integrity of the cache value: hash shape, 2xx status and a
// decodable body.
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

// expired reports that the cache value retention window has expired.
func (c cachedIdempotency) expired(now time.Time, ttl time.Duration) bool {
	return now.Sub(c.CompletedAt) >= ttl
}

// Snapshot restores the response snapshot from the cache value.
func (c cachedIdempotency) Snapshot() Snapshot {
	body, _ := base64.StdEncoding.DecodeString(c.BodyBase64)
	return Snapshot{Status: c.Status, ContentType: c.ContentType, Body: body}
}

// newIdempotencyID generates an attempt id.
func newIdempotencyID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("usecase: generate idempotency id: %w", err)
	}
	return id.String(), nil
}

// idempotencyCacheKey assembles a response cache key.
func idempotencyCacheKey(merchantID, key string) string {
	return idempotencyCachePrefix + merchantID + ":" + key
}

// derefInt dereferences an optional integer.
func derefInt(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

// derefString dereferences an optional string.
func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
