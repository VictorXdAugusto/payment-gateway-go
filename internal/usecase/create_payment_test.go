package usecase_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/idempotency"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres/pgtest"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/usecase"
)

func TestCreate_Approved_AuthorizesAtThePSP(t *testing.T) {
	e := setup(t)
	out, err := e.create.Execute(e.ctx, e.in("k1", 5000))
	if err != nil {
		t.Fatal(err)
	}
	if out.Replayed || out.Payment.Status != "authorized" || out.Payment.Amount != 5000 ||
		out.Payment.Currency != "BRL" || out.Payment.ID == "" || out.Payment.FailureReason != "" {
		t.Errorf("saída inesperada: %+v", out)
	}

	p := e.load(t, out.Payment.ID)
	if p.Status() != payment.StatusAuthorized || p.PSPReference() == "" {
		t.Errorf("estado gravado: %s ref=%q", p.Status(), p.PSPReference())
	}
	if calls, _, distinct, _ := e.psp.stats(); calls != 1 || distinct != 1 {
		t.Errorf("chamadas ao PSP=%d autorizações=%d, want 1 e 1", calls, distinct)
	}
}

func TestCreate_Declined_PersistsAFailedPayment(t *testing.T) {
	e := setup(t)
	e.psp.script(modeDecline)

	out, err := e.create.Execute(e.ctx, e.in("k1", 4000))
	if err != nil {
		t.Fatalf("recusa do PSP é um desfecho do negócio, não erro da API: %v", err)
	}
	if out.Payment.Status != "failed" || out.Payment.FailureReason != "insufficient_funds" {
		t.Errorf("view = %+v", out.Payment)
	}

	// A recusa também é replayed: o retry NÃO volta ao PSP para tentar de novo.
	again, err := e.create.Execute(e.ctx, e.in("k1", 4000))
	if err != nil || !again.Replayed || again.Payment != out.Payment {
		t.Errorf("replay da recusa: %+v / %v", again, err)
	}
	if calls, _, _, _ := e.psp.stats(); calls != 1 {
		t.Errorf("PSP acionado %d vezes, want 1", calls)
	}
}

// PSP em timeout: NÃO se assume falha. O lojista vê "processing"; internamente é unknown.
func TestCreate_IndeterminatePSP_LeavesThePaymentUnknown(t *testing.T) {
	for name, mode := range map[string]authMode{"aprovou mas não soubemos": modeTimeoutHappened, "PSP nunca recebeu": modeTimeoutLost} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.psp.script(mode)

			out, err := e.create.Execute(e.ctx, e.in("k1", 5000))
			if err != nil {
				t.Fatal(err)
			}
			if out.Payment.Status != usecase.StatusProcessing {
				t.Errorf("status público = %q, want %q", out.Payment.Status, usecase.StatusProcessing)
			}
			if got := e.load(t, out.Payment.ID).Status(); got != payment.StatusUnknown {
				t.Errorf("status interno = %s, want unknown", got)
			}
		})
	}
}

func TestCreate_UnexpectedPSPFailure_ChangesNothing_AndTheRetryRecovers(t *testing.T) {
	e := setup(t)
	e.psp.script(modeUnexpectedFailure)

	if _, err := e.create.Execute(e.ctx, e.in("k1", 5000)); err == nil {
		t.Fatal("erro de contrato com o PSP deveria falhar a requisição")
	}
	if got := e.load(t, "pay_1").Status(); got != payment.StatusCreated {
		t.Fatalf("status = %s: erro inesperado não pode decidir nada sobre o pagamento", got)
	}

	out, err := e.create.Execute(e.ctx, e.in("k1", 5000))
	if err != nil {
		t.Fatalf("o retry deveria assumir a chave liberada: %v", err)
	}
	if out.Payment.ID != "pay_1" || out.Payment.Status != "authorized" || e.count(t, "payments") != 1 {
		t.Errorf("out=%+v pagamentos=%d", out.Payment, e.count(t, "payments"))
	}
}

func TestCreate_Retry_ReplaysTheOriginalResponse(t *testing.T) {
	e := setup(t)
	first, err := e.create.Execute(e.ctx, e.in("k1", 5000))
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		again, err := e.create.Execute(e.ctx, e.in("k1", 5000))
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
	if calls, _, _, _ := e.psp.stats(); calls != 1 {
		t.Errorf("replay chamou o PSP: %d chamadas, want 1", calls)
	}
}

func TestCreate_SameKeyDifferentBody_IsRejected(t *testing.T) {
	e := setup(t)
	if _, err := e.create.Execute(e.ctx, e.in("k1", 5000)); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]usecase.CreatePaymentInput{
		"valor diferente": e.in("k1", 5001),
		"moeda diferente": {MerchantID: e.merchant, IdempotencyKey: "k1", Amount: 5000, Currency: "USD"},
	} {
		if _, err := e.create.Execute(e.ctx, in); !errors.Is(err, idempotency.ErrKeyMismatch) {
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
		if _, err := e.create.Execute(e.ctx, in); !errors.Is(err, usecase.ErrInvalidInput) {
			t.Errorf("%+v: err = %v, want ErrInvalidInput", in, err)
		}
	}
	if _, err := e.create.Execute(e.ctx, e.in("", 100)); !errors.Is(err, idempotency.ErrInvalidKey) {
		t.Errorf("chave vazia: err = %v", err)
	}
	if e.count(t, "idempotency_keys") != 0 {
		t.Fatal("requisição inválida consumiu a chave")
	}
	if calls, _, _, _ := e.psp.stats(); calls != 0 {
		t.Errorf("requisição inválida chegou ao PSP (%d chamadas)", calls)
	}

	// O cliente corrige o corpo e reusa a mesma chave: tem que funcionar.
	if _, err := e.create.Execute(e.ctx, e.in("k1", 100)); err != nil {
		t.Fatalf("a chave deveria estar livre: %v", err)
	}
}

func TestCreate_SameKeyForDifferentMerchants_IsIndependent(t *testing.T) {
	e := setup(t)
	other := pgtest.Merchant(e.ctx, t, e.pool, "outra")

	a, err := e.create.Execute(e.ctx, e.in("k1", 100))
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.create.Execute(e.ctx, usecase.CreatePaymentInput{MerchantID: other, IdempotencyKey: "k1", Amount: 100, Currency: "BRL"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Payment.ID == b.Payment.ID || b.Replayed || e.count(t, "payments") != 2 {
		t.Errorf("lojistas diferentes não podem compartilhar chave: %+v / %+v", a, b)
	}
}

// 40 clientes disparando a MESMA requisição ao mesmo tempo. Nenhum erro além de "em andamento",
// todos os sucessos enxergam o MESMO pagamento, e o PSP recebe UMA autorização.
func TestCreate_ConcurrentSameKey_CreatesAndAuthorizesExactlyOnce(t *testing.T) {
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
			out, err := e.create.Execute(e.ctx, e.in("racy", 7777))
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
	if calls, _, distinct, _ := e.psp.stats(); calls != 1 || distinct != 1 {
		t.Errorf("PSP: %d chamadas e %d autorizações, want 1 e 1", calls, distinct)
	}
	t.Logf("fresh=%d replay=%d em-andamento=%d", fresh.Load(), replayed.Load(), inFlight.Load())

	final, err := e.create.Execute(e.ctx, e.in("racy", 7777))
	if err != nil || !final.Replayed {
		t.Fatalf("retry final: %+v / %v", final, err)
	}
	if _, ok := ids[final.Payment.ID]; !ok {
		t.Errorf("retry final devolveu outro pagamento: %s", final.Payment.ID)
	}
}

// ---------------------------------------------------- quedas no meio do caminho

// Falha NA fase 1 (depois do INSERT, antes do Advance): a transação inteira desfaz, nada
// sobra, o PSP nem foi chamado, e o retry cria o pagamento UMA vez.
func TestCreate_CrashInPhase1_RollsBackAndRetryCreatesOnce(t *testing.T) {
	e := setup(t)
	e.keys.failAdvanceN.Store(1)

	if _, err := e.create.Execute(e.ctx, e.in("k1", 100)); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v, want falha injetada", err)
	}
	if e.count(t, "payments") != 0 {
		t.Fatal("o INSERT do pagamento sobreviveu à falha do Advance: fase não é atômica")
	}
	if calls, _, _, _ := e.psp.stats(); calls != 0 {
		t.Fatalf("PSP acionado %d vezes antes de o pagamento existir", calls)
	}

	out, err := e.create.Execute(e.ctx, e.in("k1", 100))
	if err != nil {
		t.Fatalf("o retry deveria assumir na hora (Release): %v", err)
	}
	if out.Replayed || e.count(t, "payments") != 1 {
		t.Errorf("replayed=%v pagamentos=%d", out.Replayed, e.count(t, "payments"))
	}
}

// O CENÁRIO-CHAVE DO CASE. O PSP APROVOU, mas o processo caiu antes de gravarmos o resultado.
// O retry retoma do recovery point, chama o PSP de novo com a MESMA chave (o id do pagamento)
// e recebe a MESMA autorização. Resultado: uma autorização, um pagamento, zero cobrança dupla.
func TestCreate_CrashAfterPSPApproved_RetryGetsTheSameAuthorization(t *testing.T) {
	e := setup(t)
	e.keys.failAdvanceN.Store(2) // 1º Advance (fase 1) passa; o 2º (gravar o desfecho) falha

	if _, err := e.create.Execute(e.ctx, e.in("k1", 5000)); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v", err)
	}
	if got := e.load(t, "pay_1").Status(); got != payment.StatusCreated {
		t.Fatalf("status = %s: o desfecho não foi gravado, o pagamento tem que continuar 'created'", got)
	}
	if _, _, distinct, _ := e.psp.stats(); distinct != 1 {
		t.Fatalf("o PSP tem %d autorizações, want 1 (ele aprovou de verdade)", distinct)
	}

	out, err := e.create.Execute(e.ctx, e.in("k1", 5000))
	if err != nil {
		t.Fatal(err)
	}
	calls, _, distinct, _ := e.psp.stats()
	if calls != 2 || distinct != 1 {
		t.Errorf("chamadas ao PSP=%d autorizações=%d: o retry chama de novo (2), mas NÃO pode duplicar (1)", calls, distinct)
	}
	if out.Payment.ID != "pay_1" || out.Payment.Status != "authorized" || e.count(t, "payments") != 1 {
		t.Errorf("out=%+v pagamentos=%d", out.Payment, e.count(t, "payments"))
	}
}

// Falha DEPOIS do desfecho gravado (na hora de finalizar): o retry retoma no ponto
// psp_resolved e NÃO volta ao PSP nem cria outro pagamento.
func TestCreate_CrashBeforeFinish_ResumesWithoutCallingThePSPAgain(t *testing.T) {
	e := setup(t)
	e.keys.failFinish.Store(1)

	if _, err := e.create.Execute(e.ctx, e.in("k1", 100)); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v", err)
	}
	var point string
	if err := e.pool.QueryRow(e.ctx, `SELECT recovery_point FROM idempotency_keys WHERE key = 'k1'`).Scan(&point); err != nil || point != "psp_resolved" {
		t.Fatalf("recovery point = %q (%v), want psp_resolved", point, err)
	}

	out, err := e.create.Execute(e.ctx, e.in("k1", 100))
	if err != nil {
		t.Fatal(err)
	}
	if calls, _, _, _ := e.psp.stats(); calls != 1 {
		t.Errorf("PSP acionado %d vezes, want 1: o desfecho já estava gravado", calls)
	}
	if e.count(t, "payments") != 1 || out.Payment.ID != "pay_1" {
		t.Errorf("pagamentos=%d id=%s", e.count(t, "payments"), out.Payment.ID)
	}
	if again, _ := e.create.Execute(e.ctx, e.in("k1", 100)); !again.Replayed || again.Payment != out.Payment {
		t.Errorf("depois de terminar, o replay deve devolver a mesma resposta: %+v", again)
	}
}

// O processo MORRE (nenhum Release). O lock fica preso até o lease vencer.
func TestCreate_ProcessDeath_ResumesAfterLeaseExpiry(t *testing.T) {
	e := setup(t)
	e.keys.failFinish.Store(1)
	e.keys.noRelease.Store(true)

	if _, err := e.create.Execute(e.ctx, e.in("k1", 100)); err == nil {
		t.Fatal("deveria falhar")
	}
	if _, err := e.create.Execute(e.ctx, e.in("k1", 100)); !errors.Is(err, idempotency.ErrInFlight) {
		t.Fatalf("com o dono 'vivo' (lease vigente): err = %v, want ErrInFlight", err)
	}

	e.expireLease(t, "k1")
	out, err := e.create.Execute(e.ctx, e.in("k1", 100))
	if err != nil {
		t.Fatalf("depois do lease deveria retomar: %v", err)
	}
	if e.count(t, "payments") != 1 || out.Payment.ID != "pay_1" {
		t.Errorf("pagamentos=%d id=%s: a retomada não pode duplicar", e.count(t, "payments"), out.Payment.ID)
	}
	if _, _, distinct, _ := e.psp.stats(); distinct != 1 {
		t.Errorf("autorizações no PSP = %d, want 1", distinct)
	}
}
