package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/ledger"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres/pgtest"
)

var now = time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)

type env struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	txm      *postgres.TxManager
	repo     *postgres.LedgerRepository
	merchant string
	seq      atomic.Int64
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := pgtest.New(t)
	ctx := context.Background()
	txm := postgres.NewTxManager(pool)
	return &env{
		ctx: ctx, pool: pool, txm: txm,
		repo:     postgres.NewLedgerRepository(txm),
		merchant: pgtest.Merchant(ctx, t, pool, "loja"),
	}
}

func brl(t testing.TB, cents int64) money.Money {
	t.Helper()
	m, err := money.New(cents, money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (e *env) capture(t testing.TB, payment string, cents, bps int64) ledger.Transaction {
	t.Helper()
	tx, err := ledger.Capture(ledger.TransactionID(fmt.Sprintf("tx_%d", e.seq.Add(1))), payment, e.merchant, brl(t, cents), bps, now)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func (e *env) refund(t testing.TB, refundID, payment string, cents int64) ledger.Transaction {
	t.Helper()
	tx, err := ledger.Refund(ledger.TransactionID(fmt.Sprintf("tx_%d", e.seq.Add(1))), refundID, payment, e.merchant, brl(t, cents), now)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func (e *env) balance(t testing.TB, acc ledger.Account) int64 {
	t.Helper()
	b, err := e.repo.Balance(e.ctx, acc.ID)
	if err != nil {
		t.Fatalf("Balance(%s): %v", acc.ID, err)
	}
	amt, err := b.Amount()
	if err != nil {
		t.Fatal(err)
	}
	return amt.Amount()
}

// trialBalance é a "prova dos noves" da contabilidade: somando TODOS os lançamentos
// do banco (débitos - créditos), o resultado tem que ser zero. Sempre.
func (e *env) trialBalance(t testing.TB) int64 {
	t.Helper()
	var diff int64
	err := e.pool.QueryRow(e.ctx, `
		SELECT COALESCE(SUM(CASE direction WHEN 'debit' THEN amount ELSE -amount END), 0)::bigint
		  FROM ledger_entries`).Scan(&diff)
	if err != nil {
		t.Fatal(err)
	}
	return diff
}

func (e *env) count(t testing.TB, table string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPost_CaptureThenRefund_DerivesBalances(t *testing.T) {
	e := setup(t)

	if err := e.repo.Post(e.ctx, e.capture(t, "pay_1", 10000, 290)); err != nil {
		t.Fatal(err)
	}
	if err := e.repo.Post(e.ctx, e.refund(t, "rf_1", "pay_1", 4000)); err != nil {
		t.Fatal(err)
	}

	if got := e.balance(t, ledger.PSPClearing(money.BRL)); got != 6000 {
		t.Errorf("psp_clearing = %d, want 6000", got)
	}
	if got := e.balance(t, ledger.MerchantBalance(e.merchant, money.BRL)); got != 5710 {
		t.Errorf("lojista = %d, want 5710", got)
	}
	if got := e.balance(t, ledger.FeeRevenue(money.BRL)); got != 290 {
		t.Errorf("taxa = %d, want 290", got)
	}
	if got := e.trialBalance(t); got != 0 {
		t.Errorf("balancete = %d, want 0", got)
	}
}

func TestBalance_UnknownAccount(t *testing.T) {
	e := setup(t)
	if _, err := e.repo.Balance(e.ctx, "nao_existe"); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestPost_DuplicateReference_PostsOnlyOnce(t *testing.T) {
	e := setup(t)
	if err := e.repo.Post(e.ctx, e.capture(t, "pay_1", 10000, 290)); err != nil {
		t.Fatal(err)
	}

	// Mesmo movimento reenviado (retry), com outro id de transação.
	err := e.repo.Post(e.ctx, e.capture(t, "pay_1", 10000, 290))
	if !errors.Is(err, ledger.ErrDuplicateReference) {
		t.Fatalf("err = %v, want ErrDuplicateReference", err)
	}
	if got := e.balance(t, ledger.PSPClearing(money.BRL)); got != 10000 {
		t.Errorf("psp_clearing = %d: o retry não pode dobrar o dinheiro", got)
	}
	if e.count(t, "ledger_transactions") != 1 || e.count(t, "ledger_entries") != 3 {
		t.Error("retry deixou lixo no banco")
	}
}

// Defesa em profundidade: mesmo que alguém escreva SQL na mão e ignore o domínio,
// o banco recusa um movimento que não fecha.
func TestDB_RejectsUnbalancedTransaction(t *testing.T) {
	e := setup(t)

	err := pgx.BeginFunc(e.ctx, e.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(e.ctx, `INSERT INTO ledger_accounts (id, type, currency) VALUES ('a', 'asset', 'BRL')`); err != nil {
			return err
		}
		if _, err := tx.Exec(e.ctx, `INSERT INTO ledger_transactions (id, reference, kind, payment_id, currency, created_at)
			VALUES ('t', 'capture:x', 'capture', 'x', 'BRL', now())`); err != nil {
			return err
		}
		_, err := tx.Exec(e.ctx, `INSERT INTO ledger_entries (transaction_id, account_id, direction, amount, currency)
			VALUES ('t', 'a', 'debit', 100, 'BRL')`) // débito sem crédito
		return err
	})
	if err == nil {
		t.Fatal("o banco aceitou uma transação desbalanceada")
	}
	if e.count(t, "ledger_entries") != 0 || e.count(t, "ledger_transactions") != 0 {
		t.Error("commit falhou mas deixou dados")
	}
}

func TestDB_IsAppendOnly(t *testing.T) {
	e := setup(t)
	if err := e.repo.Post(e.ctx, e.capture(t, "pay_1", 10000, 290)); err != nil {
		t.Fatal(err)
	}

	for _, stmt := range []string{
		`UPDATE ledger_entries SET amount = 1`,
		`DELETE FROM ledger_entries`,
		`UPDATE ledger_transactions SET kind = 'refund'`,
		`DELETE FROM ledger_transactions`,
		`TRUNCATE ledger_entries`,
		`TRUNCATE ledger_transactions CASCADE`,
	} {
		if _, err := e.pool.Exec(e.ctx, stmt); err == nil {
			t.Errorf("o banco permitiu: %s", stmt)
		}
	}
	if e.count(t, "ledger_entries") != 3 {
		t.Error("lançamentos foram alterados")
	}
}

func TestDB_RejectsEntryInWrongCurrency(t *testing.T) {
	e := setup(t)

	err := pgx.BeginFunc(e.ctx, e.pool, func(tx pgx.Tx) error {
		for _, q := range []string{
			`INSERT INTO ledger_accounts (id, type, currency) VALUES ('a', 'asset', 'BRL')`,
			`INSERT INTO ledger_transactions (id, reference, kind, payment_id, currency, created_at) VALUES ('t', 'r', 'capture', 'x', 'USD', now())`,
			`INSERT INTO ledger_entries (transaction_id, account_id, direction, amount, currency) VALUES ('t', 'a', 'debit', 100, 'USD')`,
		} {
			if _, err := tx.Exec(e.ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		t.Fatal("lançamento em USD numa conta BRL foi aceito")
	}
}

func TestPost_IsAtomic_RollsBackEverythingOnFailure(t *testing.T) {
	e := setup(t)

	// Sabota: a conta do lojista já existe no banco, mas como USD. A captura em BRL vai
	// gravar a transação e o lançamento no psp_clearing, e só então falhar no do lojista.
	sabotage := ledger.MerchantBalance(e.merchant, money.BRL)
	if _, err := e.pool.Exec(e.ctx,
		`INSERT INTO ledger_accounts (id, type, currency, merchant_id) VALUES ($1, 'liability', 'USD', $2::uuid)`,
		string(sabotage.ID), e.merchant); err != nil {
		t.Fatal(err)
	}

	if err := e.repo.Post(e.ctx, e.capture(t, "pay_1", 10000, 290)); err == nil {
		t.Fatal("Post deveria falhar")
	}

	if n := e.count(t, "ledger_transactions"); n != 0 {
		t.Errorf("sobrou %d transação(ões) depois do erro", n)
	}
	if n := e.count(t, "ledger_entries"); n != 0 {
		t.Errorf("sobrou %d lançamento(s) depois do erro", n)
	}
	if _, err := e.repo.Balance(e.ctx, ledger.PSPClearing(money.BRL).ID); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Errorf("conta criada dentro da transação sobreviveu ao rollback: %v", err)
	}
}

// O que o passo 4 vai usar: várias operações no mesmo WithinTx são tudo-ou-nada.
func TestWithinTx_MultipleRepoCallsAreAllOrNothing(t *testing.T) {
	e := setup(t)
	boom := errors.New("falha depois do primeiro Post")

	err := e.txm.WithinTx(e.ctx, func(ctx context.Context) error {
		if err := e.repo.Post(ctx, e.capture(t, "pay_1", 10000, 290)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if e.count(t, "ledger_transactions") != 0 {
		t.Fatal("o Post sobreviveu ao rollback da transação externa")
	}

	err = e.txm.WithinTx(e.ctx, func(ctx context.Context) error {
		if err := e.repo.Post(ctx, e.capture(t, "pay_2", 500, 290)); err != nil {
			return err
		}
		return e.repo.Post(ctx, e.refund(t, "rf_2", "pay_2", 200))
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.count(t, "ledger_transactions") != 2 {
		t.Fatal("as duas transações deveriam ter sido commitadas juntas")
	}
}

// Sem UPDATE de saldo não há lost update: 60 lançamentos concorrentes na MESMA conta,
// misturando capturas e estornos (que tocam as contas em ordens diferentes).
func TestPost_ConcurrentMovements_KeepLedgerConsistent(t *testing.T) {
	e := setup(t)
	const captures = 40
	const refunds = 20

	var wg sync.WaitGroup
	errs := make(chan error, captures+refunds)
	for i := 0; i < captures; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- e.repo.Post(e.ctx, e.capture(t, fmt.Sprintf("pay_%d", i), 10000, 290))
		}()
	}
	for i := 0; i < refunds; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- e.repo.Post(e.ctx, e.refund(t, fmt.Sprintf("rf_%d", i), fmt.Sprintf("pay_%d", i), 1000))
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("Post concorrente falhou: %v", err)
		}
	}

	if got, want := e.balance(t, ledger.PSPClearing(money.BRL)), int64(captures*10000-refunds*1000); got != want {
		t.Errorf("psp_clearing = %d, want %d", got, want)
	}
	if got, want := e.balance(t, ledger.MerchantBalance(e.merchant, money.BRL)), int64(captures*9710-refunds*1000); got != want {
		t.Errorf("lojista = %d, want %d", got, want)
	}
	if got, want := e.balance(t, ledger.FeeRevenue(money.BRL)), int64(captures*290); got != want {
		t.Errorf("taxa = %d, want %d", got, want)
	}
	if got := e.trialBalance(t); got != 0 {
		t.Errorf("balancete = %d, want 0", got)
	}
}

// O mesmo movimento disparado 25 vezes ao mesmo tempo (retries agressivos): entra UMA vez.
func TestPost_ConcurrentDuplicates_PostExactlyOnce(t *testing.T) {
	e := setup(t)
	const attempts = 25

	var wg sync.WaitGroup
	var ok, dup atomic.Int64
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch err := e.repo.Post(e.ctx, e.capture(t, "pay_same", 10000, 290)); {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ledger.ErrDuplicateReference):
				dup.Add(1)
			default:
				t.Errorf("erro inesperado: %v", err)
			}
		}()
	}
	wg.Wait()

	if ok.Load() != 1 || dup.Load() != attempts-1 {
		t.Fatalf("ok=%d dup=%d, want 1 e %d", ok.Load(), dup.Load(), attempts-1)
	}
	if got := e.balance(t, ledger.PSPClearing(money.BRL)); got != 10000 {
		t.Errorf("psp_clearing = %d: cobrou mais de uma vez", got)
	}
}

// Mesma referência com valor diferente não é retry: só o primeiro movimento foi contabilizado.
func TestPost_SameReferenceDifferentContent_IsConflict(t *testing.T) {
	e := setup(t)
	if err := e.repo.Post(e.ctx, e.refund(t, "r1", "pay_1", 1000)); err != nil {
		t.Fatal(err)
	}

	err := e.repo.Post(e.ctx, e.refund(t, "r1", "pay_1", 5000))
	if !errors.Is(err, ledger.ErrReferenceConflict) || errors.Is(err, ledger.ErrDuplicateReference) {
		t.Fatalf("valor diferente: err = %v, want ErrReferenceConflict", err)
	}
	err = e.repo.Post(e.ctx, e.refund(t, "r1", "pay_outro", 1000))
	if !errors.Is(err, ledger.ErrReferenceConflict) {
		t.Fatalf("pagamento diferente: err = %v, want ErrReferenceConflict", err)
	}
	// O idêntico continua sendo repetição legítima.
	err = e.repo.Post(e.ctx, e.refund(t, "r1", "pay_1", 1000))
	if !errors.Is(err, ledger.ErrDuplicateReference) {
		t.Fatalf("idêntico: err = %v, want ErrDuplicateReference", err)
	}
	if e.count(t, "ledger_transactions") != 1 || e.count(t, "ledger_entries") != 2 {
		t.Error("o conflito deixou lixo no banco")
	}
}

func TestPost_ExistingAccountWithDifferentOwner_IsMismatch(t *testing.T) {
	e := setup(t)
	other := pgtest.Merchant(e.ctx, t, e.pool, "outra")
	acc := ledger.MerchantBalance(e.merchant, money.BRL)
	if _, err := e.pool.Exec(e.ctx,
		`INSERT INTO ledger_accounts (id, type, currency, merchant_id) VALUES ($1, 'liability', 'BRL', $2::uuid)`,
		string(acc.ID), other); err != nil {
		t.Fatal(err)
	}

	err := e.repo.Post(e.ctx, e.capture(t, "pay_1", 10000, 290))
	if !errors.Is(err, ledger.ErrAccountMismatch) {
		t.Fatalf("err = %v, want ErrAccountMismatch", err)
	}
	if e.count(t, "ledger_transactions") != 0 {
		t.Error("a transação sobreviveu ao erro")
	}
}

func TestBalance_FillsMerchantID(t *testing.T) {
	e := setup(t)
	if err := e.repo.Post(e.ctx, e.capture(t, "pay_1", 10000, 290)); err != nil {
		t.Fatal(err)
	}
	b, err := e.repo.Balance(e.ctx, ledger.MerchantBalance(e.merchant, money.BRL).ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Account.MerchantID != e.merchant {
		t.Errorf("MerchantID = %q, want %q", b.Account.MerchantID, e.merchant)
	}
	p, err := e.repo.Balance(e.ctx, ledger.PSPClearing(money.BRL).ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Account.MerchantID != "" {
		t.Errorf("conta do gateway com MerchantID = %q", p.Account.MerchantID)
	}
}
