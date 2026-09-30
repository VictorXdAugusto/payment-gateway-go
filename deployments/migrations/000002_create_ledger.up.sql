-- Plano de contas. Cada conta guarda UMA moeda.
CREATE TABLE ledger_accounts (
    id          TEXT        PRIMARY KEY,
    type        TEXT        NOT NULL CHECK (type IN ('asset', 'liability', 'revenue')),
    currency    CHAR(3)     NOT NULL,
    merchant_id UUID        REFERENCES merchants (id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, currency)   -- alvo da FK composta que amarra moeda do lançamento à moeda da conta
);

-- Um movimento completo. `reference` é a chave de idempotência do dinheiro:
-- o mesmo movimento ("capture:pay_1") só pode ser lançado uma vez.
CREATE TABLE ledger_transactions (
    id         TEXT        PRIMARY KEY,
    reference  TEXT        NOT NULL UNIQUE,
    kind       TEXT        NOT NULL CHECK (kind IN ('capture', 'refund')),
    payment_id TEXT        NOT NULL,
    currency   CHAR(3)     NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (id, currency)   -- alvo da FK composta: todo lançamento tem a moeda da transação
);
CREATE INDEX ledger_transactions_payment_idx ON ledger_transactions (payment_id);

-- As pernas do movimento. Valor sempre positivo; a direção diz o lado.
CREATE TABLE ledger_entries (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id TEXT    NOT NULL,
    account_id     TEXT    NOT NULL,
    direction      TEXT    NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount         BIGINT  NOT NULL CHECK (amount > 0),
    currency       CHAR(3) NOT NULL,
    FOREIGN KEY (transaction_id, currency) REFERENCES ledger_transactions (id, currency),
    FOREIGN KEY (account_id, currency)     REFERENCES ledger_accounts (id, currency)
);
CREATE INDEX ledger_entries_account_idx     ON ledger_entries (account_id);
CREATE INDEX ledger_entries_transaction_idx ON ledger_entries (transaction_id);

-- INVARIANTE NO BANCO: por transação, soma dos débitos == soma dos créditos.
-- É um constraint trigger DIFERIDO: só é avaliado no COMMIT, quando todas as pernas
-- já foram inseridas (no meio da transação ela ainda está incompleta, e isso é normal).
CREATE FUNCTION ledger_assert_balanced() RETURNS trigger AS $$
DECLARE
    diff BIGINT;
BEGIN
    SELECT COALESCE(SUM(CASE direction WHEN 'debit' THEN amount ELSE -amount END), 0)
      INTO diff
      FROM ledger_entries
     WHERE transaction_id = NEW.transaction_id;

    IF diff <> 0 THEN
        RAISE EXCEPTION 'transação % desbalanceada (débitos - créditos = %)', NEW.transaction_id, diff
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER ledger_entries_balanced
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_assert_balanced();

-- APPEND-ONLY: o livro-razão nunca é editado nem apagado. Correção = novo lançamento inverso.
CREATE FUNCTION ledger_forbid_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'ledger é append-only: % em % não é permitido', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_forbid_mutation();
CREATE TRIGGER ledger_transactions_append_only
    BEFORE UPDATE OR DELETE ON ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_forbid_mutation();
CREATE TRIGGER ledger_entries_no_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_forbid_mutation();
CREATE TRIGGER ledger_transactions_no_truncate
    BEFORE TRUNCATE ON ledger_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_forbid_mutation();
