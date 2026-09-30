package usecase_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/idempotency"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres/pgtest"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/psp"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/usecase"
)

func (e *env) voidIn(id, key string) usecase.VoidPaymentInput {
	return usecase.VoidPaymentInput{MerchantID: e.merchant, PaymentID: id, IdempotencyKey: key}
}

func TestVoid_CancelsAnAuthorizationWithoutMovingMoney(t *testing.T) {
	e := setup(t)
	id := e.created(t, "create-1", 10000)

	out, err := e.void.Execute(e.ctx, e.voidIn(id, "void-1"))
	if err != nil || out.Replayed || out.Payment.Status != "voided" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if got := e.load(t, id).Status(); got != payment.StatusVoided {
		t.Errorf("status = %s", got)
	}
	e.assertLedger(t, 0, 0, 0) // nada foi cobrado, nada entra no ledger
	want := []string{"payment.created", "payment.authorized", "payment.voided"}
	if got := e.eventTypes(t); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("eventos = %v, want %v", got, want)
	}
}

func TestVoid_Retry_ReplaysWithoutTouchingThePSP(t *testing.T) {
	e := setup(t)
	id := e.created(t, "create-1", 10000)
	first, err := e.void.Execute(e.ctx, e.voidIn(id, "void-1"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		again, err := e.void.Execute(e.ctx, e.voidIn(id, "void-1"))
		if err != nil || !again.Replayed || again.Payment != first.Payment {
			t.Fatalf("retry %d: %+v / %v", i, again, err)
		}
	}
	if calls, distinct := e.psp.voidStats(); calls != 1 || distinct != 1 {
		t.Errorf("PSP: %d chamadas, %d cancelamentos; want 1 e 1", calls, distinct)
	}
}

func TestVoid_OnlyAuthorizedPaymentsCanBeVoided(t *testing.T) {
	e := setup(t)
	captured := e.created(t, "c1", 10000)
	if _, err := e.capture.Execute(e.ctx, e.capIn(captured, "cap")); err != nil {
		t.Fatal(err)
	}
	e.psp.script(modeDecline)
	declined := e.created(t, "c2", 10000) // failed

	for name, id := range map[string]string{"capturado": captured, "recusado": declined} {
		if _, err := e.void.Execute(e.ctx, e.voidIn(id, "void-"+name)); !errors.Is(err, payment.ErrInvalidTransition) {
			t.Errorf("%s: err = %v, want ErrInvalidTransition", name, err)
		}
	}
	if calls, _ := e.psp.voidStats(); calls != 0 {
		t.Errorf("o PSP foi acionado %d vezes para um cancelamento impossível", calls)
	}
}

func TestVoid_OtherMerchantsPayment_IsNotFound(t *testing.T) {
	e := setup(t)
	id := e.created(t, "create-1", 10000)
	other := pgtest.Merchant(e.ctx, t, e.pool, "outra-loja")

	_, err := e.void.Execute(e.ctx, usecase.VoidPaymentInput{MerchantID: other, PaymentID: id, IdempotencyKey: "v"})
	if !errors.Is(err, payment.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestVoid_PSPDeclines_LeavesThePaymentAuthorized(t *testing.T) {
	e := setup(t)
	id := e.created(t, "create-1", 10000)
	e.psp.scriptVoid(&psp.DeclinedError{Code: "already_captured"})

	if _, err := e.void.Execute(e.ctx, e.voidIn(id, "void-1")); !errors.Is(err, usecase.ErrVoidRejected) {
		t.Fatalf("err = %v", err)
	}
	if got := e.load(t, id).Status(); got != payment.StatusAuthorized {
		t.Errorf("status = %s", got)
	}
}

func TestVoid_PSPUnavailable_ChangesNothing_AndTheRetryCompletes(t *testing.T) {
	e := setup(t)
	id := e.created(t, "create-1", 10000)
	e.psp.scriptVoid(fmt.Errorf("%w: timeout", psp.ErrIndeterminate))

	if _, err := e.void.Execute(e.ctx, e.voidIn(id, "void-1")); !errors.Is(err, usecase.ErrPSPUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if got := e.load(t, id).Status(); got != payment.StatusAuthorized {
		t.Fatalf("status = %s", got)
	}
	if _, err := e.void.Execute(e.ctx, e.voidIn(id, "void-1")); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, distinct := e.psp.voidStats(); distinct != 1 {
		t.Errorf("cancelamentos distintos no PSP = %d, want 1", distinct)
	}
}

// Cair depois de o PSP cancelar e antes de gravar: o retry repete no PSP (idempotente) e conclui.
func TestVoid_CrashBeforeFinish_RollsBackEverythingAndTheRetryCompletes(t *testing.T) {
	e := setup(t)
	id := e.created(t, "create-1", 10000)
	eventsBefore := e.count(t, "outbox_events")
	e.keys.failFinish.Store(1)

	if _, err := e.void.Execute(e.ctx, e.voidIn(id, "void-1")); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v", err)
	}
	if got := e.load(t, id).Status(); got != payment.StatusAuthorized {
		t.Fatalf("status = %s: Finish está na transação, tudo devia ter desfeito", got)
	}
	if n := e.count(t, "outbox_events"); n != eventsBefore {
		t.Fatalf("eventos = %d, want %d", n, eventsBefore)
	}

	if _, err := e.void.Execute(e.ctx, e.voidIn(id, "void-1")); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if calls, distinct := e.psp.voidStats(); calls != 2 || distinct != 1 {
		t.Errorf("PSP: %d chamadas, %d cancelamentos; want 2 e 1", calls, distinct)
	}
}

func TestVoid_InvalidKey_IsRejectedBeforeAnyEffect(t *testing.T) {
	e := setup(t)
	id := e.created(t, "create-1", 10000)
	if _, err := e.void.Execute(e.ctx, e.voidIn(id, "")); !errors.Is(err, idempotency.ErrInvalidKey) {
		t.Fatalf("err = %v", err)
	}
}

// Cancelar e capturar AO MESMO TEMPO o mesmo pagamento: exatamente um vence (lock otimista),
// e o ledger só tem lançamento se a captura venceu.
func TestVoid_ConcurrentWithCapture_ExactlyOneWins(t *testing.T) {
	e := setup(t)
	for round := 0; round < 10; round++ {
		id := e.created(t, fmt.Sprintf("create-%d", round), 10000)
		var wins, voids, captures atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if _, err := e.void.Execute(e.ctx, e.voidIn(id, fmt.Sprintf("v-%d", round))); err == nil {
				wins.Add(1)
				voids.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if _, err := e.capture.Execute(e.ctx, e.capIn(id, fmt.Sprintf("c-%d", round))); err == nil {
				wins.Add(1)
				captures.Add(1)
			}
		}()
		close(start)
		wg.Wait()

		if wins.Load() != 1 {
			t.Fatalf("rodada %d: %d vencedores (void=%d capture=%d), want exatamente 1", round, wins.Load(), voids.Load(), captures.Load())
		}
		want := payment.StatusCaptured
		if voids.Load() == 1 {
			want = payment.StatusVoided
		}
		if got := e.load(t, id).Status(); got != want {
			t.Fatalf("rodada %d: status = %s, want %s", round, got, want)
		}
	}
	if got := e.trialBalance(t); got != 0 {
		t.Errorf("balancete = %d, want 0", got)
	}
}

// A chave do PSP é a do PAGAMENTO, não a do cliente: se o cliente desiste e tenta de novo com
// OUTRA Idempotency-Key depois de um timeout, o PSP reconhece o mesmo cancelamento e não há
// dois cancelamentos (nem um erro por "já cancelada").
func TestVoid_RetryWithAnotherClientKey_IsTheSameVoidAtThePSP(t *testing.T) {
	e := setup(t)
	id := e.created(t, "create-1", 10000)
	e.psp.scriptVoidHappenedThenTimeout() // o PSP cancelou, a resposta se perdeu

	if _, err := e.void.Execute(e.ctx, e.voidIn(id, "void-A")); !errors.Is(err, usecase.ErrPSPUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.void.Execute(e.ctx, e.voidIn(id, "void-B")); err != nil {
		t.Fatalf("retry com outra chave: %v", err)
	}
	if _, distinct := e.psp.voidStats(); distinct != 1 {
		t.Errorf("cancelamentos distintos no PSP = %d, want 1", distinct)
	}
}
