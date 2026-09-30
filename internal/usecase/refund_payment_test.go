package usecase_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/idempotency"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/psp"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/usecase"
)

func (e *env) refIn(id, key string, amount int64) usecase.RefundPaymentInput {
	return usecase.RefundPaymentInput{MerchantID: e.merchant, PaymentID: id, Amount: amount, IdempotencyKey: key}
}

// captured cria e captura um pagamento de R$ 100,00 (lojista 97,10; taxa 2,90).
func (e *env) captured(t *testing.T, key string, amount int64) string {
	t.Helper()
	id := e.created(t, "create-"+key, amount)
	if _, err := e.capture.Execute(e.ctx, e.capIn(id, "cap-"+key)); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRefund_Partial_ReducesWhatWeOweTheMerchantAndKeepsTheFee(t *testing.T) {
	e := setup(t)
	id := e.captured(t, "a", 10000)

	out, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-1", 4000))
	if err != nil || out.Payment.Status != "partially_refunded" || out.Payment.RefundedAmount != 4000 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	// Lojista: 9710 - 4000. PSP nos deve 10000 - 4000. A taxa NÃO é devolvida (política do case).
	e.assertLedger(t, 6000, 5710, 290)
}

func TestRefund_InTwoSteps_ReachesRefundedAndEmitsEachEvent(t *testing.T) {
	e := setup(t)
	id := e.captured(t, "a", 10000)

	if _, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-1", 4000)); err != nil {
		t.Fatal(err)
	}
	out, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-2", 6000))
	if err != nil || out.Payment.Status != "refunded" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	// Devolveu tudo: o lojista fica devendo a taxa (9710 - 10000 = -290), o PSP zera.
	e.assertLedger(t, 0, -290, 290)
	types := e.eventTypes(t)
	if n := len(types); n < 2 || types[n-1] != "payment.refunded" || types[n-2] != "payment.refunded" {
		t.Errorf("eventos = %v, want dois payment.refunded no fim", types)
	}
}

func TestRefund_CannotExceedWhatWasCaptured_AndNeverReachesThePSP(t *testing.T) {
	e := setup(t)
	id := e.captured(t, "a", 10000)
	if _, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-1", 7000)); err != nil {
		t.Fatal(err)
	}

	_, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-2", 3001))
	if !errors.Is(err, payment.ErrRefundExceedsCaptured) {
		t.Fatalf("err = %v, want ErrRefundExceedsCaptured", err)
	}
	if calls, distinct, total := e.psp.refundStats(); calls != 1 || distinct != 1 || total != 7000 {
		t.Errorf("PSP: %d chamadas, %d estornos, %d devolvidos; want 1, 1, 7000", calls, distinct, total)
	}
	e.assertLedger(t, 3000, 2710, 290)
}

func TestRefund_Retry_ReplaysWithoutRefundingTwice(t *testing.T) {
	e := setup(t)
	id := e.captured(t, "a", 10000)
	first, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-1", 4000))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		again, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-1", 4000))
		if err != nil || !again.Replayed || again.Payment != first.Payment {
			t.Fatalf("retry %d: %+v / %v", i, again, err)
		}
	}
	if calls, distinct, total := e.psp.refundStats(); calls != 1 || distinct != 1 || total != 4000 {
		t.Errorf("PSP: %d chamadas, %d estornos, %d devolvidos", calls, distinct, total)
	}
	e.assertLedger(t, 6000, 5710, 290)
}

// Mesma chave com valor diferente é uso errado do cliente, nunca um segundo estorno.
func TestRefund_SameKeyDifferentAmount_IsRejected(t *testing.T) {
	e := setup(t)
	id := e.captured(t, "a", 10000)
	if _, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-1", 4000)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-1", 5000)); !errors.Is(err, idempotency.ErrKeyMismatch) {
		t.Fatalf("err = %v, want ErrKeyMismatch", err)
	}
}

func TestRefund_InvalidAmount_DoesNotConsumeTheKey(t *testing.T) {
	e := setup(t)
	id := e.captured(t, "a", 10000)
	for _, amount := range []int64{0, -5} {
		if _, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-1", amount)); !errors.Is(err, usecase.ErrInvalidInput) {
			t.Fatalf("amount %d: err = %v, want ErrInvalidInput", amount, err)
		}
	}
	if _, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-1", 1000)); err != nil {
		t.Fatalf("a chave não devia ter sido consumida: %v", err)
	}
}

func TestRefund_OnlyCapturedPaymentsCanBeRefunded(t *testing.T) {
	e := setup(t)
	authorized := e.created(t, "c1", 10000)
	if _, err := e.refund.Execute(e.ctx, e.refIn(authorized, "ref-1", 100)); !errors.Is(err, payment.ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	if calls, _, _ := e.psp.refundStats(); calls != 0 {
		t.Errorf("o PSP foi acionado %d vezes para um estorno impossível", calls)
	}
}

func TestRefund_PSPDeclines_ChangesNothing(t *testing.T) {
	e := setup(t)
	id := e.captured(t, "a", 10000)
	e.psp.scriptRefund(&psp.DeclinedError{Code: "refund_exceeds_captured"})

	if _, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-1", 1000)); !errors.Is(err, usecase.ErrRefundRejected) {
		t.Fatalf("err = %v", err)
	}
	if got := e.load(t, id).Status(); got != payment.StatusCaptured {
		t.Errorf("status = %s", got)
	}
	e.assertLedger(t, 10000, 9710, 290)
}

func TestRefund_PSPUnavailable_ChangesNothing_AndTheRetryCompletesOnce(t *testing.T) {
	e := setup(t)
	id := e.captured(t, "a", 10000)
	e.psp.scriptRefund(fmt.Errorf("%w: timeout", psp.ErrIndeterminate))

	if _, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-1", 4000)); !errors.Is(err, usecase.ErrPSPUnavailable) {
		t.Fatalf("err = %v", err)
	}
	e.assertLedger(t, 10000, 9710, 290)

	if _, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-1", 4000)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, distinct, total := e.psp.refundStats(); distinct != 1 || total != 4000 {
		t.Errorf("PSP: %d estornos, %d devolvidos; want 1 e 4000", distinct, total)
	}
	e.assertLedger(t, 6000, 5710, 290)
}

// O dinheiro já voltou no PSP, mas o ledger falhou: nada pode ficar pela metade, e o retry
// (mesma chave do PSP) fecha os dois lados sem devolver duas vezes.
func TestRefund_LedgerFailure_RollsBackThePaymentUpdate_AndTheRetryHealsIt(t *testing.T) {
	e := setup(t)
	id := e.captured(t, "a", 10000)
	e.ledger.fail.Store(true)

	if _, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-1", 4000)); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v", err)
	}
	if got := e.load(t, id); got.Status() != payment.StatusCaptured || !got.RefundedAmount().IsZero() {
		t.Fatalf("o pagamento ficou estornado sem ledger: %+v", got.Snapshot())
	}

	e.ledger.fail.Store(false)
	if _, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-1", 4000)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if calls, distinct, total := e.psp.refundStats(); calls != 2 || distinct != 1 || total != 4000 {
		t.Errorf("PSP: %d chamadas, %d estornos, %d devolvidos; want 2, 1, 4000", calls, distinct, total)
	}
	e.assertLedger(t, 6000, 5710, 290)
}

func TestRefund_CrashBeforeFinish_RollsBackEverything(t *testing.T) {
	e := setup(t)
	id := e.captured(t, "a", 10000)
	eventsBefore := e.count(t, "outbox_events")
	e.keys.failFinish.Store(1)

	if _, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-1", 4000)); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v", err)
	}
	e.assertLedger(t, 10000, 9710, 290)
	if n := e.count(t, "outbox_events"); n != eventsBefore {
		t.Fatalf("eventos = %d, want %d", n, eventsBefore)
	}
}

// 10 estornos de R$ 10,00 ao mesmo tempo sobre um pagamento de R$ 100,00, chaves diferentes.
// Quem perde a corrida do lock otimista leva conflito e o PSP JÁ devolveu: o retry com a mesma
// chave cura. No fim, a soma devolvida no PSP, no pagamento e no ledger é a MESMA e nunca
// passa do capturado.
func TestRefund_ConcurrentDifferentKeys_NeverOverRefunds_AndRetriesHeal(t *testing.T) {
	e := setup(t)
	id := e.captured(t, "a", 10000)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			key := fmt.Sprintf("ref-%d", i)
			for attempt := 0; attempt < 50; attempt++ {
				_, err := e.refund.Execute(e.ctx, e.refIn(id, key, 1000))
				if err == nil {
					return
				}
				if !errors.Is(err, payment.ErrConcurrentModification) && !errors.Is(err, idempotency.ErrInFlight) {
					t.Errorf("estorno %d: erro inesperado: %v", i, err)
					return
				}
			}
			t.Errorf("estorno %d nunca concluiu", i)
		}()
	}
	close(start)
	wg.Wait()

	p := e.load(t, id)
	if p.Status() != payment.StatusRefunded || p.RefundedAmount().Amount() != 10000 {
		t.Fatalf("pagamento: %+v", p.Snapshot())
	}
	if _, distinct, total := e.psp.refundStats(); distinct != 10 || total != 10000 {
		t.Errorf("PSP: %d estornos, %d devolvidos; want 10 e 10000", distinct, total)
	}
	e.assertLedger(t, 0, -290, 290)
}
