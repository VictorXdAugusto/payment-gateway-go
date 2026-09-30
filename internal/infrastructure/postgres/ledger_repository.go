package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

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
			return r.classifyDuplicate(ctx, db, t)
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

// classifyDuplicate decide o que a referência repetida significa: repetição idempotente
// (mesmo conteúdo financeiro) ou conflito (mesma referência, outro movimento).
func (r *LedgerRepository) classifyDuplicate(ctx context.Context, db DBTX, t ledger.Transaction) error {
	var kind, paymentID string
	if err := db.QueryRow(ctx,
		`SELECT kind, payment_id FROM ledger_transactions WHERE reference = $1`, t.Reference()).Scan(&kind, &paymentID); err != nil {
		return fmt.Errorf("carregar transação existente: %w", err)
	}
	if kind != string(t.Kind()) || paymentID != t.PaymentID() {
		return fmt.Errorf("%w: %s gravada como %s do pagamento %s, nova chamada é %s do pagamento %s",
			ledger.ErrReferenceConflict, t.Reference(), kind, paymentID, t.Kind(), t.PaymentID())
	}

	rows, err := db.Query(ctx, `
		SELECT e.account_id, e.direction, e.amount, e.currency
		  FROM ledger_entries e
		  JOIN ledger_transactions t ON t.id = e.transaction_id
		 WHERE t.reference = $1`, t.Reference())
	if err != nil {
		return fmt.Errorf("carregar lançamentos existentes: %w", err)
	}
	defer rows.Close()
	var stored []string
	for rows.Next() {
		var acc, dir, cur string
		var amount int64
		if err := rows.Scan(&acc, &dir, &amount, &cur); err != nil {
			return err
		}
		stored = append(stored, legKey(acc, dir, amount, cur))
	}
	if err := rows.Err(); err != nil {
		return err
	}

	var incoming []string
	for _, e := range t.Entries() {
		incoming = append(incoming, legKey(string(e.Account.ID), string(e.Direction), e.Amount.Amount(), string(e.Amount.Currency())))
	}
	sort.Strings(stored)
	sort.Strings(incoming)
	if !slices.Equal(stored, incoming) {
		return fmt.Errorf("%w: %s já gravada com lançamentos %v, nova chamada tem %v",
			ledger.ErrReferenceConflict, t.Reference(), stored, incoming)
	}
	return ledger.ErrDuplicateReference
}

// legKey é a forma canônica de uma perna (conta, direção, valor, moeda) para comparação.
func legKey(account, direction string, amount int64, currency string) string {
	return fmt.Sprintf("%s|%s|%d|%s", account, direction, amount, currency)
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
	tag, err := db.Exec(ctx, `
		INSERT INTO ledger_accounts (id, type, currency, merchant_id)
		VALUES ($1, $2, $3, $4::uuid)
		ON CONFLICT (id) DO NOTHING`,
		string(a.ID), string(a.Type), string(a.Currency), merchantID)
	if err != nil {
		return fmt.Errorf("garantir conta %s: %w", a.ID, err)
	}
	if tag.RowsAffected() > 0 {
		return nil // conta criada agora, nada a conferir
	}

	// A conta já existia: só serve se for a mesma (tipo, moeda e dono).
	var same bool
	if err := db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM ledger_accounts
			 WHERE id = $1 AND type = $2 AND currency = $3
			   AND merchant_id IS NOT DISTINCT FROM $4::uuid)`,
		string(a.ID), string(a.Type), string(a.Currency), merchantID).Scan(&same); err != nil {
		return fmt.Errorf("conferir conta %s: %w", a.ID, err)
	}
	if !same {
		return fmt.Errorf("%w: conta %s", ledger.ErrAccountMismatch, a.ID)
	}
	return nil
}

func (r *LedgerRepository) Balance(ctx context.Context, id ledger.AccountID) (ledger.Balance, error) {
	var (
		acc            = ledger.Account{ID: id}
		typ, cur, mid  string
		debits, credit int64
	)
	err := r.tx.DB(ctx).QueryRow(ctx, `
		SELECT a.type, a.currency, COALESCE(a.merchant_id::text, ''),
		       COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'debit'),  0)::bigint,
		       COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'credit'), 0)::bigint
		  FROM ledger_accounts a
		  LEFT JOIN ledger_entries e ON e.account_id = a.id
		 WHERE a.id = $1
		 GROUP BY a.id`, string(id)).Scan(&typ, &cur, &mid, &debits, &credit)
	if errors.Is(err, pgx.ErrNoRows) {
		return ledger.Balance{}, ledger.ErrAccountNotFound
	}
	if err != nil {
		return ledger.Balance{}, fmt.Errorf("saldo de %s: %w", id, err)
	}

	acc.Type, acc.Currency, acc.MerchantID = ledger.AccountType(typ), money.Currency(cur), mid
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

// MerchantBalances soma os lançamentos das contas do lojista, uma linha por moeda.
func (r *LedgerRepository) MerchantBalances(ctx context.Context, merchantID string) ([]ledger.Balance, error) {
	rows, err := r.tx.DB(ctx).Query(ctx, `
		SELECT a.id, a.currency,
		       COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'debit'),  0)::bigint,
		       COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'credit'), 0)::bigint
		  FROM ledger_accounts a
		  LEFT JOIN ledger_entries e ON e.account_id = a.id
		 WHERE a.merchant_id = $1::uuid
		 GROUP BY a.id, a.currency
		 ORDER BY a.currency`, merchantID)
	if err != nil {
		return nil, fmt.Errorf("saldos do lojista: %w", err)
	}
	defer rows.Close()

	var out []ledger.Balance
	for rows.Next() {
		var (
			id, cur        string
			debits, credit int64
		)
		if err := rows.Scan(&id, &cur, &debits, &credit); err != nil {
			return nil, err
		}
		acc := ledger.MerchantBalance(merchantID, money.Currency(cur))
		d, err := money.New(debits, acc.Currency)
		if err != nil {
			return nil, err
		}
		c, err := money.New(credit, acc.Currency)
		if err != nil {
			return nil, err
		}
		out = append(out, ledger.Balance{Account: acc, Debits: d, Credits: c})
	}
	return out, rows.Err()
}

// MerchantStatement pagina por cursor (id do lançamento), sempre do mais novo para o mais antigo.
// O índice (account_id, id DESC) atende a consulta sem ordenar.
func (r *LedgerRepository) MerchantStatement(ctx context.Context, merchantID string, before int64, limit int) ([]ledger.StatementLine, error) {
	rows, err := r.tx.DB(ctx).Query(ctx, `
		SELECT e.id, e.transaction_id, t.kind, t.payment_id, e.direction, e.amount, e.currency, t.created_at
		  FROM ledger_entries e
		  JOIN ledger_transactions t ON t.id = e.transaction_id
		 WHERE e.account_id IN (SELECT id FROM ledger_accounts WHERE merchant_id = $1::uuid)
		   AND ($2::bigint = 0 OR e.id < $2)
		 ORDER BY e.id DESC
		 LIMIT $3`, merchantID, before, limit)
	if err != nil {
		return nil, fmt.Errorf("extrato do lojista: %w", err)
	}
	defer rows.Close()

	var out []ledger.StatementLine
	for rows.Next() {
		var (
			l               ledger.StatementLine
			kind, dir, cur  string
			amount          int64
			created         time.Time
			txID, paymentID string
		)
		if err := rows.Scan(&l.EntryID, &txID, &kind, &paymentID, &dir, &amount, &cur, &created); err != nil {
			return nil, err
		}
		m, err := money.New(amount, money.Currency(cur))
		if err != nil {
			return nil, err
		}
		l.TransactionID, l.Kind, l.PaymentID = ledger.TransactionID(txID), ledger.Kind(kind), paymentID
		l.Direction, l.Amount, l.CreatedAt = ledger.Direction(dir), m, created.UTC()
		out = append(out, l)
	}
	return out, rows.Err()
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
