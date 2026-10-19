-- +goose Up
-- Merchant API invoice projection (7.4): the BFF owns the merchant context of
-- an invoice (merchant_order_id, metadata, the requested amount) that the
-- payment service does not store (CBL-17.14, D1). The rows are append-only in
-- 7.4: the invoice state lives in Payment and is projected on read.
CREATE TABLE invoices (
    payment_id        uuid        PRIMARY KEY,
    merchant_id       text        NOT NULL CHECK (merchant_id ~ '^[A-Za-z0-9_-]{1,128}$'),
    merchant_order_id text        NOT NULL DEFAULT '' CHECK (merchant_order_id = '' OR char_length(merchant_order_id) <= 128),
    metadata          jsonb       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    amount_minor      bigint      NOT NULL DEFAULT 0 CHECK (amount_minor >= 0),
    amount_asset      text        NOT NULL DEFAULT '' CHECK (amount_asset = '' OR char_length(amount_asset) <= 16),
    amount_decimals   smallint    NOT NULL DEFAULT 0 CHECK (amount_decimals BETWEEN 0 AND 18),
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX invoices_merchant_created_idx ON invoices (merchant_id, created_at DESC, payment_id DESC);
CREATE INDEX invoices_merchant_order_idx ON invoices (merchant_id, merchant_order_id) WHERE merchant_order_id <> '';

-- Create intents (7.4): the stable per-key expiry anchor. The derived Payment
-- idempotency key is stable across retries, so every retry must send the same
-- expires_at; the intent row pins it per (merchant, Idempotency-Key). Rows are
-- transient (a retention job is a 14.2 candidate).
CREATE TABLE invoice_intents (
    merchant_id     text        NOT NULL CHECK (merchant_id ~ '^[A-Za-z0-9_-]{1,128}$'),
    idempotency_key text        NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9._~-]{8,128}$'),
    expires_at      timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, idempotency_key)
);

-- +goose Down
DROP TABLE invoice_intents;
DROP TABLE invoices;
