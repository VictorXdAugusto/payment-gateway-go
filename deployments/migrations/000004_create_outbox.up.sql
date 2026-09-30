-- Transactional outbox: o evento é gravado NA MESMA TRANSAÇÃO que muda o estado do negócio.
-- Se a transação desfaz, o evento some junto; se comita, o evento existe e SERÁ entregue
-- (pelo menos uma vez). Cada linha é também o estado da entrega ao webhook do lojista.
CREATE TABLE outbox_events (
    id              BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id        TEXT        NOT NULL UNIQUE,  -- vai para o lojista: é a chave de deduplicação dele
    merchant_id     UUID        NOT NULL REFERENCES merchants (id),
    type            TEXT        NOT NULL,
    payment_id      TEXT        NOT NULL,
    payload         BYTEA       NOT NULL,         -- bytes EXATOS que serão assinados e enviados
    status          TEXT        NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'delivered', 'dead', 'skipped')),
    attempts        INT         NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_until    TIMESTAMPTZ,                  -- lease do worker que pegou a entrega
    last_error      TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL,
    delivered_at    TIMESTAMPTZ,
    CHECK ((status = 'delivered') = (delivered_at IS NOT NULL))
);

-- Índice parcial: só as entregas pendentes, que é o que o worker consulta o tempo todo.
CREATE INDEX outbox_due_idx ON outbox_events (next_attempt_at, id) WHERE status = 'pending';
CREATE INDEX outbox_payment_idx ON outbox_events (payment_id);
