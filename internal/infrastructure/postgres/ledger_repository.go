package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/ledger"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
)

const pgCheckViolation = "23514" // usado pelo trigger de balanço

type LedgerRepository struct {
	tx *TxManager
}

var _ ledger.Repository = (*LedgerRepository)(nil)

func NewLedgerRepository(tx *TxManager) *LedgerRepository { return &LedgerRepository{tx: tx} }

// Post grava a transação e todas as pernas de forma atômica.
//
// Note o que NÃO existe aqui: nenhum "UPDATE saldo = saldo + x". O ledger só recebe
// INSERTs, então não há leitura-modifica-escreve, não há lock de linha de saldo e
// não há lost update: mil goroutines lançando na mesma conta não se atrapalham.
func (r *LedgerRepository) Post(ctx context.Context, t ledger.Transaction) error {
	err := r.tx.WithinTx(ctx, func(ctx context.Context) error {
		db := r.tx.DB(ctx)

		// Idempotência do dinheiro: a mesma referência só entra uma vez.
		tag, err := db.Exec(ctx, `
			INSERT INTO ledger_transactions (id, reference, kind, payment_id, currency, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (reference) DO NOTHING`,
			string(t.ID()), t.Reference(), string(t.Kind()), t.PaymentID(), string(t.Currency()), t.CreatedAt())
		if err != nil {
			return fmt.Errorf("inserir transação: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ledger.ErrDuplicateReference
		}

		// Contas sempre em ordem de ID. Precaução clássica contra deadlock: duas transações
		// que criam contas novas em ordens opostas (captura toca psp->lojista, estorno
		// lojista->psp) poderiam se esperar mutuamente. Não consegui reproduzir o problema
		// sem a ordenação (a janela é só a criação da conta), então é defesa, não correção.
		entries := t.Entries()
		for _, a := range sortedAccounts(entries) {
			if err := r.ensureAccount(ctx, db, a); err != nil {
				return err
			}
		}

		for _, e := range entries {
			if _, err := db.Exec(ctx, `
				INSERT INTO ledger_entries (transaction_id, account_id, direction, amount, currency)
				VALUES ($1, $2, $3, $4, $5)`,
				string(t.ID()), string(e.Account.ID), string(e.Direction), e.Amount.Amount(), string(e.Amount.Currency())); err != nil {
				return fmt.Errorf("inserir lançamento: %w", err)
			}
		}
		return nil
	})
	return mapLedgerError(err)
}

func sortedAccounts(entries []ledger.Entry) []ledger.Account {
	seen := make(map[ledger.AccountID]ledger.Account, len(entries))
	for _, e := range entries {
		seen[e.Account.ID] = e.Account
	}
	out := make([]ledger.Account, 0, len(seen))
	for _, a := range seen {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ensureAccount cria a conta na primeira vez que ela aparece (ex.: primeiro pagamento de um lojista).
func (r *LedgerRepository) ensureAccount(ctx context.Context, db DBTX, a ledger.Account) error {
	var merchantID any // NULL nas contas do gateway
	if a.MerchantID != "" {
		merchantID = a.MerchantID
	}
	_, err := db.Exec(ctx, `
		INSERT INTO ledger_accounts (id, type, currency, merchant_id)
		VALUES ($1, $2, $3, $4::uuid)
		ON CONFLICT (id) DO NOTHING`,
		string(a.ID), string(a.Type), string(a.Currency), merchantID)
	if err != nil {
		return fmt.Errorf("garantir conta %s: %w", a.ID, err)
	}
	return nil
}

func (r *LedgerRepository) Balance(ctx context.Context, id ledger.AccountID) (ledger.Balance, error) {
	var (
		acc            = ledger.Account{ID: id}
		typ, cur       string
		debits, credit int64
	)
	err := r.tx.DB(ctx).QueryRow(ctx, `
		SELECT a.type, a.currency,
		       COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'debit'),  0)::bigint,
		       COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'credit'), 0)::bigint
		  FROM ledger_accounts a
		  LEFT JOIN ledger_entries e ON e.account_id = a.id
		 WHERE a.id = $1
		 GROUP BY a.id`, string(id)).Scan(&typ, &cur, &debits, &credit)
	if errors.Is(err, pgx.ErrNoRows) {
		return ledger.Balance{}, ledger.ErrAccountNotFound
	}
	if err != nil {
		return ledger.Balance{}, fmt.Errorf("saldo de %s: %w", id, err)
	}

	acc.Type, acc.Currency = ledger.AccountType(typ), money.Currency(cur)
	d, err := money.New(debits, acc.Currency)
	if err != nil {
		return ledger.Balance{}, err
	}
	c, err := money.New(credit, acc.Currency)
	if err != nil {
		return ledger.Balance{}, err
	}
	return ledger.Balance{Account: acc, Debits: d, Credits: c}, nil
}

// mapLedgerError traduz a violação do trigger de balanço para o erro de domínio.
// Só dispara se o código passar por cima da validação do domínio (defesa em profundidade).
func mapLedgerError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgCheckViolation {
		return fmt.Errorf("%w: %s", ledger.ErrUnbalanced, pgErr.Message)
	}
	return err
}
