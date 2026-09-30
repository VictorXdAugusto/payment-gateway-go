package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/ledger"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/idempotency"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres/pgtest"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/outbox"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/psp"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/usecase"
)

var (
	t0          = time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	errInjected = errors.New("falha injetada")
)

// ---------------------------------------------------------------- PSP falso

type authMode int

const (
	modeApprove           authMode = iota // aprova
	modeDecline                           // recusa (402)
	modeTimeoutHappened                   // aprova NO PSP, mas a resposta se perde (timeout)
	modeTimeoutLost                       // timeout e o PSP nunca recebeu
	modeUnexpectedFailure                 // erro comum (bug de contrato)
)

// fakePSP é idempotente por chave, como o PSP de verdade, e conta as chamadas para os
// testes provarem QUANTAS vezes o PSP foi acionado e QUANTAS autorizações distintas existem.
type fakePSP struct {
	mu       sync.Mutex
	auths    map[string]string // idempotency key -> referência
	declined map[string]string // idempotency key -> código
	captured map[string]bool   // idempotency key da captura
	voided   map[string]bool   // idempotency key do cancelamento
	refunded map[string]int64  // idempotency key do estorno -> valor (estorno idempotente)
	modes    []authMode        // roteiro consumido a cada NOVA autorização (vazio = aprova)

	authorizeCalls, captureCalls, lookupCalls int
	voidCalls, refundCalls                    int
	captureErrs                               []error // roteiro da captura; nil = sucesso
	voidErrs, refundErrs                      []error // roteiros do cancelamento e do estorno
	voidHappenedThenTimeout                   bool    // o PSP cancela MAS a resposta se perde (o caso perigoso)
	lookupErr                                 error
}

func newFakePSP() *fakePSP {
	return &fakePSP{auths: map[string]string{}, declined: map[string]string{}, captured: map[string]bool{},
		voided: map[string]bool{}, refunded: map[string]int64{}}
}

func (f *fakePSP) script(m ...authMode) { f.mu.Lock(); f.modes = m; f.mu.Unlock() }

func (f *fakePSP) scriptCapture(errs ...error) { f.mu.Lock(); f.captureErrs = errs; f.mu.Unlock() }

// stats devolve chamadas totais e quantidades DISTINTAS (autorizações e capturas).
func (f *fakePSP) stats() (authorizeCalls, captureCalls, distinctAuths, distinctCaptures int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authorizeCalls, f.captureCalls, len(f.auths), len(f.captured)
}

func (f *fakePSP) lookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookupCalls
}

func (f *fakePSP) Authorize(_ context.Context, req psp.AuthorizeRequest) (psp.Authorization, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authorizeCalls++

	if ref, ok := f.auths[req.IdempotencyKey]; ok { // replay idempotente
		return psp.Authorization{Reference: ref}, nil
	}
	if code, ok := f.declined[req.IdempotencyKey]; ok {
		return psp.Authorization{}, &psp.DeclinedError{Code: code, Message: code}
	}

	mode := modeApprove
	if len(f.modes) > 0 {
		mode, f.modes = f.modes[0], f.modes[1:]
	}
	switch mode {
	case modeDecline:
		f.declined[req.IdempotencyKey] = "insufficient_funds"
		return psp.Authorization{}, &psp.DeclinedError{Code: "insufficient_funds", Message: "saldo insuficiente"}
	case modeTimeoutLost:
		return psp.Authorization{}, fmt.Errorf("%w: timeout", psp.ErrIndeterminate)
	case modeUnexpectedFailure:
		return psp.Authorization{}, errors.New("PSP rejeitou a requisição (400)")
	}
	ref := fmt.Sprintf("auth_%d", len(f.auths)+1)
	f.auths[req.IdempotencyKey] = ref
	if mode == modeTimeoutHappened {
		return psp.Authorization{}, fmt.Errorf("%w: timeout", psp.ErrIndeterminate)
	}
	return psp.Authorization{Reference: ref}, nil
}

func (f *fakePSP) Capture(_ context.Context, req psp.CaptureRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captureCalls++
	if f.captured[req.IdempotencyKey] {
		return nil
	}
	if len(f.captureErrs) > 0 {
		err := f.captureErrs[0]
		f.captureErrs = f.captureErrs[1:]
		if err != nil {
			return err
		}
	}
	f.captured[req.IdempotencyKey] = true
	return nil
}

func (f *fakePSP) scriptVoid(errs ...error) { f.mu.Lock(); f.voidErrs = errs; f.mu.Unlock() }

// scriptVoidHappenedThenTimeout: a próxima chamada de cancelamento acontece no PSP, mas o
// chamador só vê um timeout.
func (f *fakePSP) scriptVoidHappenedThenTimeout() {
	f.mu.Lock()
	f.voidHappenedThenTimeout = true
	f.mu.Unlock()
}

func (f *fakePSP) scriptRefund(errs ...error) { f.mu.Lock(); f.refundErrs = errs; f.mu.Unlock() }

// refundStats devolve as chamadas e quanto dinheiro foi REALMENTE devolvido (chaves distintas).
func (f *fakePSP) refundStats() (calls int, distinct int, total int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.refunded {
		total += v
	}
	return f.refundCalls, len(f.refunded), total
}

func (f *fakePSP) voidStats() (calls, distinct int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.voidCalls, len(f.voided)
}

func (f *fakePSP) Void(_ context.Context, req psp.VoidRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.voidCalls++
	if f.voided[req.IdempotencyKey] {
		return nil
	}
	if f.voidHappenedThenTimeout {
		f.voidHappenedThenTimeout = false
		f.voided[req.IdempotencyKey] = true
		return fmt.Errorf("%w: timeout", psp.ErrIndeterminate)
	}
	if len(f.voidErrs) > 0 {
		err := f.voidErrs[0]
		f.voidErrs = f.voidErrs[1:]
		if err != nil {
			return err
		}
	}
	f.voided[req.IdempotencyKey] = true
	return nil
}

func (f *fakePSP) Refund(_ context.Context, req psp.RefundRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refundCalls++
	if _, ok := f.refunded[req.IdempotencyKey]; ok {
		return nil
	}
	if len(f.refundErrs) > 0 {
		err := f.refundErrs[0]
		f.refundErrs = f.refundErrs[1:]
		if err != nil {
			return err
		}
	}
	f.refunded[req.IdempotencyKey] = req.Amount.Amount()
	return nil
}

func (f *fakePSP) Lookup(_ context.Context, key string) (psp.LookupResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookupCalls++
	if f.lookupErr != nil {
		return psp.LookupResult{}, f.lookupErr
	}
	if ref, ok := f.auths[key]; ok {
		return psp.LookupResult{Outcome: psp.OutcomeAuthorized, Reference: ref}, nil
	}
	if code, ok := f.declined[key]; ok {
		return psp.LookupResult{Outcome: psp.OutcomeDeclined, DeclineCode: code}, nil
	}
	return psp.LookupResult{Outcome: psp.OutcomeNotFound}, nil
}

// ------------------------------------------------------ injeção de falhas

// faultyKeys embrulha o store real e injeta as falhas da vida real: a fase que dá erro,
// o processo que morre antes de soltar o lock. O código de produção não sabe disso.
type faultyKeys struct {
	idempotency.Store
	advanceCalls atomic.Int32
	failAdvanceN atomic.Int32 // falha exatamente a N-ésima chamada a Advance (1-based); 0 = nunca
	failFinish   atomic.Int32 // quantas chamadas a Finish ainda vão falhar
	noRelease    atomic.Bool  // simula morte do processo: nunca solta o lock
}

func (f *faultyKeys) Advance(ctx context.Context, a idempotency.Acquisition, point, res string) error {
	if n := f.advanceCalls.Add(1); n == f.failAdvanceN.Load() {
		return errInjected
	}
	return f.Store.Advance(ctx, a, point, res)
}

func (f *faultyKeys) Finish(ctx context.Context, a idempotency.Acquisition, body []byte) error {
	if f.failFinish.Add(-1) >= 0 {
		return errInjected
	}
	return f.Store.Finish(ctx, a, body)
}

func (f *faultyKeys) Release(ctx context.Context, a idempotency.Acquisition) error {
	if f.noRelease.Load() {
		return nil
	}
	return f.Store.Release(ctx, a)
}

type faultyLedger struct {
	ledger.Repository
	fail atomic.Bool
}

func (f *faultyLedger) Post(ctx context.Context, t ledger.Transaction) error {
	if f.fail.Load() {
		return errInjected
	}
	return f.Repository.Post(ctx, t)
}

// faultyOutbox falha o Add de eventos quando pedido: prova que estado e evento andam juntos.
type faultyOutbox struct {
	outbox.Repository
	fail atomic.Bool
	hook func() bool // se != nil e devolver true, esta chamada a Add falha (definir antes de usar)
}

func (f *faultyOutbox) Add(ctx context.Context, events ...outbox.Event) error {
	if f.fail.Load() || (f.hook != nil && f.hook()) {
		return errInjected
	}
	return f.Repository.Add(ctx, events...)
}

// ------------------------------------------------------------------ ambiente

type env struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	merchant string
	keys     *faultyKeys
	ledger   *faultyLedger
	outbox   *faultyOutbox
	psp      *fakePSP
	payments *postgres.PaymentRepository

	create  *usecase.CreatePayment
	capture *usecase.CapturePayment
	void    *usecase.VoidPayment
	refund  *usecase.RefundPayment
	resolve *usecase.ResolveStuck

	clock atomic.Int64 // unix nano do "agora" controlável
}

const (
	testFeeBps = 290
	testGrace  = 2 * time.Minute
)

func setup(t *testing.T) *env {
	t.Helper()
	pool := pgtest.New(t)
	ctx := context.Background()
	txm := postgres.NewTxManager(pool)

	e := &env{
		ctx: ctx, pool: pool, psp: newFakePSP(),
		keys:     &faultyKeys{Store: postgres.NewIdempotencyStore(txm, 30*time.Second)},
		ledger:   &faultyLedger{Repository: postgres.NewLedgerRepository(txm)},
		outbox:   &faultyOutbox{Repository: postgres.NewOutboxRepository(txm)},
		payments: postgres.NewPaymentRepository(txm),
		merchant: pgtest.Merchant(ctx, t, pool, "loja"),
	}
	e.clock.Store(t0.UnixNano())
	now := func() time.Time { return time.Unix(0, e.clock.Load()).UTC() }

	var paySeq, txSeq, evtSeq atomic.Int64
	events := usecase.NewEventRecorder(e.outbox, func() string { return fmt.Sprintf("evt_%d", evtSeq.Add(1)) })
	newPayID := func() payment.ID { return payment.ID(fmt.Sprintf("pay_%d", paySeq.Add(1))) }
	newTxID := func() ledger.TransactionID { return ledger.TransactionID(fmt.Sprintf("ltx_%d", txSeq.Add(1))) }

	e.create = usecase.NewCreatePayment(txm, e.payments, e.keys, e.psp, events, newPayID, now)
	e.capture = usecase.NewCapturePayment(txm, e.payments, e.ledger, e.keys, e.psp, events, testFeeBps, newTxID, now)
	e.void = usecase.NewVoidPayment(txm, e.payments, e.keys, e.psp, events, now)
	e.refund = usecase.NewRefundPayment(txm, e.payments, e.ledger, e.keys, e.psp, events, newTxID, now)
	e.resolve = usecase.NewResolveStuck(txm, e.payments, e.psp, events, testGrace, now)
	return e
}

func (e *env) advanceClock(d time.Duration) { e.clock.Add(int64(d)) }

func (e *env) in(key string, amount int64) usecase.CreatePaymentInput {
	return usecase.CreatePaymentInput{MerchantID: e.merchant, IdempotencyKey: key, Amount: amount, Currency: "BRL"}
}

func (e *env) count(t testing.TB, table string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *env) expireLease(t testing.TB, key string) {
	t.Helper()
	if _, err := e.pool.Exec(e.ctx,
		`UPDATE idempotency_keys SET locked_at = now() - interval '1 hour' WHERE key = $1`, key); err != nil {
		t.Fatal(err)
	}
}

// created cria um pagamento pelo caso de uso e devolve o id.
func (e *env) created(t testing.TB, key string, amount int64) string {
	t.Helper()
	out, err := e.create.Execute(e.ctx, e.in(key, amount))
	if err != nil {
		t.Fatalf("criar pagamento: %v", err)
	}
	return out.Payment.ID
}

func (e *env) load(t testing.TB, id string) *payment.Payment {
	t.Helper()
	p, err := e.payments.GetByID(e.ctx, payment.ID(id))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *env) balance(t testing.TB, acc ledger.Account) int64 {
	t.Helper()
	b, err := e.ledger.Balance(e.ctx, acc.ID)
	if errors.Is(err, ledger.ErrAccountNotFound) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	amt, err := b.Amount()
	if err != nil {
		t.Fatal(err)
	}
	return amt.Amount()
}

// trialBalance: somando TODOS os lançamentos (débito - crédito) tem que dar zero. Sempre.
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

// eventTypes devolve os tipos de evento gravados na outbox, na ordem em que foram criados.
func (e *env) eventTypes(t testing.TB) []string {
	t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT type FROM outbox_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// events monta um EventRecorder novo sobre a outbox real (para testes que constroem casos de uso à mão).
func (e *env) events() *usecase.EventRecorder {
	var n atomic.Int64
	return usecase.NewEventRecorder(e.outbox, func() string { return fmt.Sprintf("evt_x%d", n.Add(1)) })
}
