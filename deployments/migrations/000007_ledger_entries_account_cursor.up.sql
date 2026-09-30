-- Extrato do lojista: lançamentos de uma conta, do mais novo para o mais antigo, paginados por
-- cursor de id. O índice composto atende a consulta sem ordenar e cobre o prefixo account_id,
-- então o índice simples anterior fica redundante.
CREATE INDEX ledger_entries_account_cursor_idx ON ledger_entries (account_id, id DESC);
DROP INDEX ledger_entries_account_idx;
