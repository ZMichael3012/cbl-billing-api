-- +goose Up
-- Идемпотентность мерчант API (7.3): источник истины - уникальный индекс
-- (merchant_id, key); снимок ответа хранится только для успешных вызовов.
-- Записи транзитные (окно IDEMPOTENCY_TTL, очистка - 14.2), поэтому DELETE
-- разрешен: при ошибке обработчика строка удаляется, мерчант повторяет.
CREATE TABLE idempotency_keys (
    id                    uuid        PRIMARY KEY,
    merchant_id           text        NOT NULL CHECK (merchant_id ~ '^[A-Za-z0-9_-]{1,128}$'),
    key                   text        NOT NULL CHECK (key ~ '^[A-Za-z0-9._~-]{8,128}$'),
    request_hash          bytea       NOT NULL CHECK (octet_length(request_hash) = 32),
    state                 text        NOT NULL CHECK (state IN ('processing', 'completed')),
    response_status       integer,
    response_content_type text,
    response_body         bytea       CHECK (response_body IS NULL OR octet_length(response_body) <= 262144),
    created_at            timestamptz NOT NULL DEFAULT now(),
    completed_at          timestamptz,
    CONSTRAINT idempotency_keys_merchant_key_uq UNIQUE (merchant_id, key),
    CHECK (state = 'processing' OR (
        response_status IS NOT NULL
        AND response_status >= 200 AND response_status < 300
        AND completed_at IS NOT NULL))
);
CREATE INDEX idempotency_created_idx ON idempotency_keys (created_at);

-- +goose StatementBegin
CREATE FUNCTION idempotency_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.merchant_id IS DISTINCT FROM OLD.merchant_id
        OR NEW.key IS DISTINCT FROM OLD.key
        OR NEW.request_hash IS DISTINCT FROM OLD.request_hash
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'idempotency_keys: immutable fields cannot change';
    END IF;
    IF OLD.state = 'completed' THEN
        RAISE EXCEPTION 'idempotency_keys: completed record is immutable';
    END IF;
    IF NEW.state IS DISTINCT FROM OLD.state AND NEW.state <> 'completed' THEN
        RAISE EXCEPTION 'idempotency_keys: only processing -> completed allowed';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp;
-- +goose StatementEnd

CREATE TRIGGER idempotency_guard_trigger
    BEFORE UPDATE ON idempotency_keys
    FOR EACH ROW EXECUTE FUNCTION idempotency_guard();

-- +goose Down
DROP TRIGGER idempotency_guard_trigger ON idempotency_keys;
DROP FUNCTION idempotency_guard();
DROP TABLE idempotency_keys;
