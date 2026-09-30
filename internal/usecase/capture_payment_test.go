package usecase_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/ledger"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/idempotency"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres/pgtest"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/psp"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/usecase"
)

func (e *env) capIn(id, key string) usecase.CapturePaymentInput {
	return usecase.CapturePaymentInput{MerchantID: e.merchant, PaymentID: id, IdempotencyKey: key}
}

func (e *env) accounts() (pspClearing, merchant, fees ledger.Account) {
	return ledger.PSPClearing(money.BRL), ledger.MerchantBalance(e.merchant, money.BRL), ledger.FeeRevenue(money.BRL)
}

// assertLedger confere os três saldos e o balancete (que tem que ser sempre zero).
func (e *env) assertLedger(t *testing.T, wantPSP, wantMerchant, wantFees int64) {
	t.Helper()
	a, m, f := e.accounts()
	if got := e.balance(t, a); got != wantPSP {
		t.Errorf("psp_clearing = %d, want %d", got, wantPSP)
	}
	if got := e.balance(t, m); got != wantMerchant {
		t.Errorf("saldo do lojista = %d, want %d", got, wantMerchant)
	}
	if got := e.balance(t, f); got != wantFees {
		t.Errorf("receita de taxas = %d, want %d", got, wantFees)
	}
	if got := e.trialBalance(t); got != 0 {
		t.Errorf("balancete = %d, want 0", got)
	}
}

func TestCapture_MovesThePaymentAndTheLedgerTogether(t *testing.T) {
	e := setup(t)
	id := e.created(t, "create-1", 10000)

	out, err := e.capture.Execute(e.ctx, e.capIn(id, "cap-1"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Replayed || out.Payment.Status != "captured" {
		t.Errorf("saída: %+v", out)
	}
	if got := e.load(t, id).Status(); got != payment.StatusCaptured {
		t.Errorf("status = %s", got)
	}
	// R$ 100,00 a 2,90%: o lojista fica com 97,10 e o gateway com 2,90.
	e.assertLedger(t, 10000, 9710, 290)
	if _, _, _, captures := e.psp.stats(); captures != 1 {
		t.Errorf("capturas no PSP = %d", captures)
	}
}

func TestCapture_Retry_ReplaysWithoutTouchingThePSPOrTheLedger(t *testing.T) {
	e := setup(t)
	id := e.created(t, "create-1", 10000)
	first, err := e.capture.Execute(e.ctx, e.capIn(id, "cap-1"))
	if err != nil {
		t.Fatal(err)
	}
	_, callsBefore, _, _ := e.psp.stats()

	for i := 0; i < 3; i++ {
		again, err := e.capture.Execute(e.ctx, e.capIn(id, "cap-1"))
		if err != nil || !again.Replayed || again.Payment != first.Payment {
			t.Fatalf("retry %d: %+v / %v", i, again, err)
		}
	}
	if _, callsAfter, _, _ := e.psp.stats(); callsAfter != callsBefore {
		t.Errorf("replay acionou o PSP (%d -> %d chamadas)", callsBefore, callsAfter)
	}
	e.assertLedger(t, 10000, 9710, 290) // continua lançado UMA vez
}

func TestCapture_OnlyAuthorizedPaymentsCanBeCaptured(t *testing.T) {
	e := setup(t)

	// Já capturado (com outra chave: o cliente tentou capturar de novo).
	captured := e.created(t, "create-1", 10000)
	if _, err := e.capture.Execute(e.ctx, e.capIn(captured, "cap-1")); err != nil {
		t.Fatal(err)
	}
	_, callsBefore, _, _ := e.psp.stats()
	var te *payment.TransitionError
	if _, err := e.capture.Execute(e.ctx, e.capIn(captured, "cap-2")); !errors.As(err, &te) || te.From != payment.StatusCaptured {
		t.Errorf("captura dupla com outra chave: err = %v", err)
	}

	// Recusado no PSP (failed).
	e.psp.script(modeDecline)
	failed := e.created(t, "create-2", 4000)
	if _, err := e.capture.Execute(e.ctx, e.capIn(failed, "cap-3")); !errors.As(err, &te) || te.From != payment.StatusFailed {
		t.Errorf("captura de pagamento failed: err = %v", err)
	}

	// Em unknown: ainda não se sabe se autorizou.
	e.psp.script(modeTimeoutLost)
	unknown := e.created(t, "create-3", 5000)
	if _, err := e.capture.Execute(e.ctx, e.capIn(unknown, "cap-4")); !errors.As(err, &te) || te.From != payment.StatusUnknown {
		t.Errorf("captura de pagamento unknown: err = %v", err)
	}

	if _, callsAfter, _, _ := e.psp.stats(); callsAfter != callsBefore {
		t.Errorf("estado inválido não pode chegar ao PSP (%d -> %d chamadas)", callsBefore, callsAfter)
	}
	e.assertLedger(t, 10000, 9710, 290) // só a primeira captura moveu dinheiro
}

func TestCapture_OtherMerchantsPayment_IsNotFound(t *testing.T) {
	e := setup(t)
	id := e.created(t, "create-1", 10000)
	intruder := pgtest.Merchant(e.ctx, t, e.pool, "intrusa")

	_, err := e.capture.Execute(e.ctx, usecase.CapturePaymentInput{MerchantID: intruder, PaymentID: id, IdempotencyKey: "cap-1"})
	if !errors.Is(err, payment.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got := e.load(t, id).Status(); got != payment.StatusAuthorized {
		t.Errorf("status = %s", got)
	}
}

// O PSP não confirmou. Nada muda aqui; repetir com a MESMA chave conclui.
func TestCapture_PSPUnavailable_ChangesNothing_AndTheRetryCompletes(t *testing.T) {
	e := setup(t)
	id := e.created(t, "create-1", 10000)
	e.psp.scriptCapture(fmt.Errorf("%w: timeout", psp.ErrIndeterminate))

	if _, err := e.capture.Execute(e.ctx, e.capIn(id, "cap-1")); !errors.Is(err, usecase.ErrPSPUnavailable) {
		t.Fatalf("err = %v, want ErrPSPUnavailable", err)
	}
	if got := e.load(t, id).Status(); got != payment.StatusAuthorized {
		t.Fatalf("status = %s: sem confirmação do PSP o pagamento continua authorized", got)
	}
	e.assertLedger(t, 0, 0, 0)

	if _, err := e.capture.Execute(e.ctx, e.capIn(id, "cap-1")); err != nil {
		t.Fatalf("o retry com a mesma chave deveria concluir: %v", err)
	}
	e.assertLedger(t, 10000, 9710, 290)
}

func TestCapture_PSPDeclines_LeavesThePaymentAuthorized(t *testing.T) {
	e := setup(t)
	id := e.created(t, "create-1", 10000)
	e.psp.scriptCapture(&psp.DeclinedError{Code: "capture_declined", Message: "recusada"})

	if _, err := e.capture.Execute(e.ctx, e.capIn(id, "cap-1")); !errors.Is(err, usecase.ErrCaptureRejected) {
		t.Fatalf("err = %v", err)
	}
	if got := e.load(t, id).Status(); got != payment.StatusAuthorized {
		t.Errorf("status = %s", got)
	}
	e.assertLedger(t, 0, 0, 0)
}

// A garantia central: pagamento capturado e ledger andam JUNTOS. Se o ledger falha, o
// pagamento NÃO fica capturado; sem "capturado sem dinheiro registrado".
func TestCapture_LedgerFailure_RollsBackThePaymentUpdate(t *testing.T) {
	e := setup(t)
	id := e.created(t, "create-1", 10000)
	e.ledger.fail.Store(true)

	if _, err := e.capture.Execute(e.ctx, e.capIn(id, "cap-1")); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v", err)
	}
	if got := e.load(t, id).Status(); got != payment.StatusAuthorized {
		t.Fatalf("status = %s: o pagamento ficou capturado sem ledger", got)
	}
	e.assertLedger(t, 0, 0, 0)

	// O PSP JÁ capturou. O retry repete a captura (idempotente) e conclui os dois lados.
	e.ledger.fail.Store(false)
	if _, err := e.capture.Execute(e.ctx, e.capIn(id, "cap-1")); err != nil {
		t.Fatalf("retry: %v", err)
	}
	_, calls, _, distinct := e.psp.stats()
	if calls != 2 || distinct != 1 {
		t.Errorf("capturas no PSP: %d chamadas, %d distintas; want 2 e 1 (o retry repete, sem duplicar)", calls, distinct)
	}
	e.assertLedger(t, 10000, 9710, 290)
}

func TestCapture_CrashBeforeFinish_CannotHappenWithoutTheRest(t *testing.T) {
	e := setup(t)
	id := e.created(t, "create-1", 10000)
	e.keys.failFinish.Store(1)

	if _, err := e.capture.Execute(e.ctx, e.capIn(id, "cap-1")); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v", err)
	}
	// Finish está DENTRO da transação: se ele falha, o ledger e o pagamento desfazem juntos.
	if got := e.load(t, id).Status(); got != payment.StatusAuthorized {
		t.Fatalf("status = %s", got)
	}
	e.assertLedger(t, 0, 0, 0)
}

// 20 requisições simultâneas com a MESMA chave: uma captura, o ledger lançado uma vez.
func TestCapture_ConcurrentSameKey_CapturesOnce(t *testing.T) {
	e := setup(t)
	id := e.created(t, "create-1", 10000)

	var wg sync.WaitGroup
	var fresh atomic.Int64
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out, err := e.capture.Execute(e.ctx, e.capIn(id, "cap-same"))
			switch {
			case err == nil && !out.Replayed:
				fresh.Add(1)
			case err == nil, errors.Is(err, idempotency.ErrInFlight):
			default:
				t.Errorf("erro inesperado: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if fresh.Load() != 1 {
		t.Errorf("%d capturas 'frescas', want 1", fresh.Load())
	}
	e.assertLedger(t, 10000, 9710, 290)
}

// Um cliente com bug dispara 10 capturas com chaves DIFERENTES para o mesmo pagamento.
// A chave de idempotência não protege aqui (são chaves distintas); quem protege é o
// lock otimista + a máquina de estados + a referência única do ledger.
func TestCapture_ConcurrentDifferentKeys_OnlyOneWins(t *testing.T) {
	e := setup(t)
	id := e.created(t, "create-1", 10000)

	var wg sync.WaitGroup
	var ok atomic.Int64
	start := make(chan struct{})
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := e.capture.Execute(e.ctx, e.capIn(id, fmt.Sprintf("cap-%d", i)))
			var te *payment.TransitionError
			switch {
			case err == nil:
				ok.Add(1)
			case errors.As(err, &te), errors.Is(err, payment.ErrConcurrentModification):
			default:
				t.Errorf("erro inesperado: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if ok.Load() != 1 {
		t.Fatalf("%d capturas tiveram sucesso, want exatamente 1", ok.Load())
	}
	if _, _, _, distinct := e.psp.stats(); distinct != 1 {
		t.Errorf("capturas distintas no PSP = %d, want 1", distinct)
	}
	e.assertLedger(t, 10000, 9710, 290)
}

// Vários pagamentos capturados: o balancete continua zerado e os saldos somam.
func TestCapture_ManyPayments_LedgerStaysBalanced(t *testing.T) {
	e := setup(t)
	total, net, fees := int64(0), int64(0), int64(0)
	for i, cents := range []int64{10000, 333, 1, 99999, 250} {
		id := e.created(t, fmt.Sprintf("create-%d", i), cents)
		if _, err := e.capture.Execute(e.ctx, e.capIn(id, fmt.Sprintf("cap-%d", i))); err != nil {
			t.Fatal(err)
		}
		amount, _ := money.New(cents, money.BRL)
		parts, err := amount.Allocate(testFeeBps, 10000-testFeeBps)
		if err != nil {
			t.Fatal(err)
		}
		total += cents
		fees += parts[0].Amount()
		net += parts[1].Amount()
	}
	e.assertLedger(t, total, net, fees)
	if total != net+fees {
		t.Fatalf("total %d != líquido %d + taxas %d: centavo perdido", total, net, fees)
	}
}
