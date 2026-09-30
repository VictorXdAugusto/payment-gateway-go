CREATE TABLE payments (
    id              TEXT        PRIMARY KEY,
    merchant_id     UUID        NOT NULL REFERENCES merchants (id),
    idempotency_key TEXT        NOT NULL,
    amount          BIGINT      NOT NULL CHECK (amount > 0),
    currency        CHAR(3)     NOT NULL,
    refunded_amount BIGINT      NOT NULL DEFAULT 0,
    status          TEXT        NOT NULL CHECK (status IN (
        'created', 'authorized', 'captured', 'partially_refunded',
        'refunded', 'voided', 'failed', 'unknown')),
    psp_reference   TEXT        NOT NULL DEFAULT '',
    failure_reason  TEXT        NOT NULL DEFAULT '',
    version         BIGINT      NOT NULL DEFAULT 0 CHECK (version >= 0),
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    CHECK (refunded_amount >= 0 AND refunded_amount <= amount),
    -- Segunda rede de proteção da idempotência: mesmo que a camada de chaves falhe,
    -- o banco não deixa o mesmo lojista criar dois pagamentos com a mesma chave.
    CONSTRAINT payments_merchant_idempotency_key UNIQUE (merchant_id, idempotency_key)
);
CREATE INDEX payments_merchant_created_idx ON payments (merchant_id, created_at DESC);

-- Registro de cada chave de idempotência recebida.
--   recovery_point: até onde o processamento já chegou (retoma dali no retry)
--   lock_token:     fencing token, incrementa a cada aquisição; quem tem token velho perde
--   locked_at:      início do lease; NULL = ninguém processando
CREATE TABLE idempotency_keys (
    merchant_id    UUID        NOT NULL REFERENCES merchants (id),
    key            TEXT        NOT NULL CHECK (length(key) BETWEEN 1 AND 255),
    request_hash   TEXT        NOT NULL,
    recovery_point TEXT        NOT NULL,
    resource_id    TEXT        NOT NULL DEFAULT '',
    response_body  BYTEA,
    lock_token     BIGINT      NOT NULL DEFAULT 1,
    locked_at      TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, key),
    -- Uma chave terminada tem resposta e não está travada; uma em andamento não tem resposta.
    CHECK ((recovery_point = 'finished') = (response_body IS NOT NULL)),
    CHECK (recovery_point <> 'finished' OR locked_at IS NULL)
);
CREATE INDEX idempotency_keys_created_idx ON idempotency_keys (created_at);
