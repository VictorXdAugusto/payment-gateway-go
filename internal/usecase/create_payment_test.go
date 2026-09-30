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

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/idempotency"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres/pgtest"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/usecase"
)

var fixedNow = time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)

// faultyKeys embrulha o store real e injeta as falhas que a vida real produz: a fase que
// dá erro, o processo que morre antes de soltar o lock. O código de produção não sabe disso.
type faultyKeys struct {
	idempotency.Store
	failAdvance atomic.Int32 // quantas chamadas a Advance ainda vão falhar
	failFinish  atomic.Int32
	noRelease   atomic.Bool // simula morte do processo: nunca solta o lock
}

var errInjected = errors.New("falha injetada")

func (f *faultyKeys) Advance(ctx context.Context, a idempotency.Acquisition, point, res string) error {
	if f.failAdvance.Add(-1) >= 0 {
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

type env struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	merchant string
	keys     *faultyKeys
	uc       *usecase.CreatePayment
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := pgtest.New(t)
	ctx := context.Background()
	txm := postgres.NewTxManager(pool)
	keys := &faultyKeys{Store: postgres.NewIdempotencyStore(txm, 30*time.Second)}

	var seq atomic.Int64
	newID := func() payment.ID { return payment.ID(fmt.Sprintf("pay_%d", seq.Add(1))) }

	return &env{
		ctx: ctx, pool: pool, keys: keys,
		merchant: pgtest.Merchant(ctx, t, pool, "loja"),
		uc:       usecase.NewCreatePayment(txm, postgres.NewPaymentRepository(txm), keys, newID, func() time.Time { return fixedNow }),
	}
}

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

func TestCreate_FirstCall_CreatesPayment(t *testing.T) {
	e := setup(t)
	out, err := e.uc.Execute(e.ctx, e.in("k1", 5000))
	if err != nil {
		t.Fatal(err)
	}
	if out.Replayed || out.Payment.Status != "created" || out.Payment.Amount != 5000 || out.Payment.Currency != "BRL" || out.Payment.ID == "" {
		t.Errorf("saída inesperada: %+v", out)
	}
	if e.count(t, "payments") != 1 {
		t.Error("deveria existir 1 pagamento")
	}
}

func TestCreate_Retry_ReplaysTheOriginalResponse(t *testing.T) {
	e := setup(t)
	first, err := e.uc.Execute(e.ctx, e.in("k1", 5000))
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		again, err := e.uc.Execute(e.ctx, e.in("k1", 5000))
		if err != nil {
			t.Fatal(err)
		}
		if !again.Replayed || again.Payment != first.Payment {
			t.Fatalf("retry %d: replayed=%v payment=%+v, want a resposta original %+v", i, again.Replayed, again.Payment, first.Payment)
		}
	}
	if e.count(t, "payments") != 1 {
		t.Fatalf("retries criaram %d pagamentos, want 1", e.count(t, "payments"))
	}
}

func TestCreate_SameKeyDifferentBody_IsRejected(t *testing.T) {
	e := setup(t)
	if _, err := e.uc.Execute(e.ctx, e.in("k1", 5000)); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]usecase.CreatePaymentInput{
		"valor diferente": e.in("k1", 5001),
		"moeda diferente": {MerchantID: e.merchant, IdempotencyKey: "k1", Amount: 5000, Currency: "USD"},
	} {
		if _, err := e.uc.Execute(e.ctx, in); !errors.Is(err, idempotency.ErrKeyMismatch) {
			t.Errorf("%s: err = %v, want ErrKeyMismatch", name, err)
		}
	}
	if e.count(t, "payments") != 1 {
		t.Error("requisição rejeitada não pode criar pagamento")
	}
}

func TestCreate_InvalidInput_DoesNotConsumeTheKey(t *testing.T) {
	e := setup(t)
	bad := []usecase.CreatePaymentInput{
		e.in("k1", 0),
		e.in("k1", -10),
		{MerchantID: e.merchant, IdempotencyKey: "k1", Amount: 100, Currency: "XYZ"},
		{MerchantID: e.merchant, IdempotencyKey: "k1", Amount: 100, Currency: ""},
	}
	for _, in := range bad {
		if _, err := e.uc.Execute(e.ctx, in); !errors.Is(err, usecase.ErrInvalidInput) {
			t.Errorf("%+v: err = %v, want ErrInvalidInput", in, err)
		}
	}
	if _, err := e.uc.Execute(e.ctx, e.in("", 100)); !errors.Is(err, idempotency.ErrInvalidKey) {
		t.Errorf("chave vazia: err = %v", err)
	}
	if e.count(t, "idempotency_keys") != 0 {
		t.Fatal("requisição inválida consumiu a chave")
	}

	// O cliente corrige o corpo e reusa a mesma chave: tem que funcionar.
	if _, err := e.uc.Execute(e.ctx, e.in("k1", 100)); err != nil {
		t.Fatalf("a chave deveria estar livre: %v", err)
	}
}

func TestCreate_SameKeyForDifferentMerchants_IsIndependent(t *testing.T) {
	e := setup(t)
	other := pgtest.Merchant(e.ctx, t, e.pool, "outra")

	a, err := e.uc.Execute(e.ctx, e.in("k1", 100))
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.uc.Execute(e.ctx, usecase.CreatePaymentInput{MerchantID: other, IdempotencyKey: "k1", Amount: 100, Currency: "BRL"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Payment.ID == b.Payment.ID || b.Replayed || e.count(t, "payments") != 2 {
		t.Errorf("lojistas diferentes não podem compartilhar chave: %+v / %+v", a, b)
	}
}

// 40 clientes disparando a MESMA requisição ao mesmo tempo (o retry agressivo de um SDK).
// Regra: nenhum erro além de "em andamento", todos os sucessos enxergam o MESMO pagamento,
// e o banco termina com exatamente UM.
func TestCreate_ConcurrentSameKey_CreatesExactlyOnce(t *testing.T) {
	e := setup(t)
	const callers = 40

	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := map[string]int{}
	var fresh, replayed, inFlight atomic.Int64
	start := make(chan struct{})

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out, err := e.uc.Execute(e.ctx, e.in("racy", 7777))
			switch {
			case err == nil:
				mu.Lock()
				ids[out.Payment.ID]++
				mu.Unlock()
				if out.Replayed {
					replayed.Add(1)
				} else {
					fresh.Add(1)
				}
			case errors.Is(err, idempotency.ErrInFlight):
				inFlight.Add(1)
			default:
				t.Errorf("erro inesperado: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if fresh.Load() != 1 {
		t.Errorf("%d execuções 'frescas', want exatamente 1", fresh.Load())
	}
	if len(ids) != 1 {
		t.Errorf("respostas com %d ids diferentes: %v", len(ids), ids)
	}
	if e.count(t, "payments") != 1 {
		t.Fatalf("banco tem %d pagamentos, want 1", e.count(t, "payments"))
	}
	t.Logf("fresh=%d replay=%d em-andamento=%d", fresh.Load(), replayed.Load(), inFlight.Load())

	// Depois que a poeira baixa, qualquer retry devolve o mesmo pagamento.
	final, err := e.uc.Execute(e.ctx, e.in("racy", 7777))
	if err != nil || !final.Replayed {
		t.Fatalf("retry final: %+v / %v", final, err)
	}
	if _, ok := ids[final.Payment.ID]; !ok {
		t.Errorf("retry final devolveu outro pagamento: %s", final.Payment.ID)
	}
}

// Falha NA fase 1 (depois do INSERT do pagamento, antes do Advance): a transação inteira
// desfaz, nada sobra, e o retry cria o pagamento UMA vez.
func TestCreate_CrashInPhase1_RollsBackAndRetryCreatesOnce(t *testing.T) {
	e := setup(t)
	e.keys.failAdvance.Store(1)

	if _, err := e.uc.Execute(e.ctx, e.in("k1", 100)); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v, want falha injetada", err)
	}
	if e.count(t, "payments") != 0 {
		t.Fatal("o INSERT do pagamento sobreviveu à falha do Advance: fase não é atômica")
	}

	out, err := e.uc.Execute(e.ctx, e.in("k1", 100))
	if err != nil {
		t.Fatalf("o retry deveria assumir na hora (Release): %v", err)
	}
	if out.Replayed || e.count(t, "payments") != 1 {
		t.Errorf("replayed=%v pagamentos=%d", out.Replayed, e.count(t, "payments"))
	}
}

// Falha DEPOIS da fase 1 commitada (na hora de finalizar): o pagamento existe. O retry
// retoma do recovery point e NÃO cria um segundo. Este é o cenário-chave da idempotência.
func TestCreate_CrashAfterPhase1_ResumesWithoutDuplicating(t *testing.T) {
	e := setup(t)
	e.keys.failFinish.Store(1)

	if _, err := e.uc.Execute(e.ctx, e.in("k1", 100)); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v", err)
	}
	if e.count(t, "payments") != 1 {
		t.Fatal("a fase 1 deveria ter sido commitada")
	}
	var point string
	if err := e.pool.QueryRow(e.ctx, `SELECT recovery_point FROM idempotency_keys WHERE key = 'k1'`).Scan(&point); err != nil || point != "payment_created" {
		t.Fatalf("recovery point = %q (%v), want payment_created", point, err)
	}

	out, err := e.uc.Execute(e.ctx, e.in("k1", 100))
	if err != nil {
		t.Fatal(err)
	}
	if e.count(t, "payments") != 1 {
		t.Fatalf("o retry criou outro pagamento: %d", e.count(t, "payments"))
	}
	if out.Payment.ID != "pay_1" {
		t.Errorf("id = %s, want o pagamento da 1ª tentativa (pay_1)", out.Payment.ID)
	}
	if again, _ := e.uc.Execute(e.ctx, e.in("k1", 100)); !again.Replayed || again.Payment != out.Payment {
		t.Errorf("depois de terminar, o replay deve devolver a mesma resposta: %+v", again)
	}
}

// O processo MORRE (nenhum Release). O lock fica preso até o lease vencer.
func TestCreate_ProcessDeath_ResumesAfterLeaseExpiry(t *testing.T) {
	e := setup(t)
	e.keys.failFinish.Store(1)
	e.keys.noRelease.Store(true)

	if _, err := e.uc.Execute(e.ctx, e.in("k1", 100)); err == nil {
		t.Fatal("deveria falhar")
	}

	if _, err := e.uc.Execute(e.ctx, e.in("k1", 100)); !errors.Is(err, idempotency.ErrInFlight) {
		t.Fatalf("com o dono 'vivo' (lease vigente): err = %v, want ErrInFlight", err)
	}

	e.expireLease(t, "k1")
	out, err := e.uc.Execute(e.ctx, e.in("k1", 100))
	if err != nil {
		t.Fatalf("depois do lease deveria retomar: %v", err)
	}
	if e.count(t, "payments") != 1 || out.Payment.ID != "pay_1" {
		t.Errorf("pagamentos=%d id=%s: a retomada não pode duplicar", e.count(t, "payments"), out.Payment.ID)
	}
}
