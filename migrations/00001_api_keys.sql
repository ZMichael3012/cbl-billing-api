-- +goose Up
-- API-ключи мерчантов (7.2): хранится только sha256 полного ключа; prefix
-- служит аудиту и поиску оператором. Частичные уникальные индексы делают
-- структурно невозможными более одного active и более одного retiring ключа
-- на мерчанта (второй барьер к логике Rotate).
CREATE TABLE api_keys (
    id           uuid        PRIMARY KEY,
    merchant_id  text        NOT NULL CHECK (merchant_id ~ '^[A-Za-z0-9_-]{1,128}$'),
    prefix       text        NOT NULL CHECK (prefix ~ '^[0-9a-f]{10}$'),
    token_hash   bytea       NOT NULL CHECK (octet_length(token_hash) = 32),
    status       text        NOT NULL CHECK (status IN ('active', 'retiring', 'revoked')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz,
    revoked_at   timestamptz,
    last_used_at timestamptz,
    -- Retiring обязан иметь срок окончания окна; revoked обязан иметь время
    -- отзыва; обратные импликации не требуются: revoked сохраняет исторический
    -- expires_at для аудита.
    CHECK (status <> 'retiring' OR expires_at IS NOT NULL),
    CHECK (status <> 'revoked' OR revoked_at IS NOT NULL)
);
CREATE UNIQUE INDEX api_keys_token_hash_idx ON api_keys (token_hash);
CREATE UNIQUE INDEX api_keys_prefix_idx ON api_keys (prefix);
CREATE UNIQUE INDEX api_keys_one_active_idx ON api_keys (merchant_id) WHERE status = 'active';
CREATE UNIQUE INDEX api_keys_one_retiring_idx ON api_keys (merchant_id) WHERE status = 'retiring';
CREATE INDEX api_keys_merchant_idx ON api_keys (merchant_id, created_at DESC);

-- +goose StatementBegin
CREATE FUNCTION api_keys_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.merchant_id IS DISTINCT FROM OLD.merchant_id
        OR NEW.prefix IS DISTINCT FROM OLD.prefix
        OR NEW.token_hash IS DISTINCT FROM OLD.token_hash
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'api_keys: immutable fields cannot change';
    END IF;
    -- Отозванный ключ неизменяем: повторный отзыв идемпотентен в сервисе,
    -- а не строкой БД.
    IF OLD.status = 'revoked' THEN
        RAISE EXCEPTION 'api_keys: revoked key is immutable';
    END IF;
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF (OLD.status, NEW.status) NOT IN (('active', 'retiring'), ('active', 'revoked'), ('retiring', 'revoked')) THEN
            RAISE EXCEPTION 'api_keys: transition % -> % is forbidden', OLD.status, NEW.status;
        END IF;
    ELSE
        -- Без смены статуса разрешено только обновление last_used_at.
        IF NEW.expires_at IS DISTINCT FROM OLD.expires_at
            OR NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN
            RAISE EXCEPTION 'api_keys: lifecycle fields require status change';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp;
-- +goose StatementEnd

CREATE TRIGGER api_keys_guard_trigger
    BEFORE UPDATE ON api_keys
    FOR EACH ROW EXECUTE FUNCTION api_keys_guard();

-- Запрет удаления и очистки: строка revoked-ключа - след отзыва, его нельзя
-- потерять (аудит операций - 12.1; journal отсутствует осознанно).
-- +goose StatementBegin
CREATE FUNCTION api_keys_no_delete() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'api_keys: % is forbidden', TG_OP;
END;
$$ LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp;
-- +goose StatementEnd

CREATE TRIGGER api_keys_no_delete_trigger
    BEFORE DELETE OR TRUNCATE ON api_keys
    FOR EACH STATEMENT EXECUTE FUNCTION api_keys_no_delete();

-- +goose Down
DROP TRIGGER api_keys_no_delete_trigger ON api_keys;
DROP FUNCTION api_keys_no_delete();
DROP TRIGGER api_keys_guard_trigger ON api_keys;
DROP FUNCTION api_keys_guard();
DROP TABLE api_keys;
