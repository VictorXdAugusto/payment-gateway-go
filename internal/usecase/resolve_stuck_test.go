package usecase_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/usecase"
)

// unknownPayment cria um pagamento que ficou em unknown por causa de um timeout no PSP.
func unknownPayment(t *testing.T, e *env, mode authMode) payment.ID {
	t.Helper()
	e.psp.script(mode)
	out, err := e.create.Execute(e.ctx, e.in("k-"+string(rune('a'+int(mode))), 5000))
	if err != nil {
		t.Fatal(err)
	}
	if got := e.load(t, out.Payment.ID).Status(); got != payment.StatusUnknown {
		t.Fatalf("preparação: status = %s, want unknown", got)
	}
	return payment.ID(out.Payment.ID)
}

// O PSP aprovou e a resposta se perdeu. A reconciliação descobre a verdade.
func TestResolveStuck_PSPHadApproved_BecomesAuthorized(t *testing.T) {
	e := setup(t)
	id := unknownPayment(t, e, modeTimeoutHappened)

	status, err := e.resolve.Execute(e.ctx, id)
	if err != nil || status != payment.StatusAuthorized {
		t.Fatalf("status=%s err=%v, want authorized", status, err)
	}
	p := e.load(t, string(id))
	if p.PSPReference() == "" || p.Status() != payment.StatusAuthorized {
		t.Errorf("a referência do PSP precisa ser gravada: %+v", p.Snapshot())
	}
	if !p.UpdatedAt().Equal(t0) {
		t.Errorf("updated_at = %v", p.UpdatedAt())
	}
}

func TestResolveStuck_PSPHadDeclined_BecomesFailed(t *testing.T) {
	e := setup(t)
	id := unknownPayment(t, e, modeTimeoutHappened)
	// O PSP acabou recusando (registrado do lado dele) enquanto ficamos sem resposta.
	e.psp.mu.Lock()
	delete(e.psp.auths, string(id))
	e.psp.declined[string(id)] = "insufficient_funds"
	e.psp.mu.Unlock()

	status, err := e.resolve.Execute(e.ctx, id)
	if err != nil || status != payment.StatusFailed {
		t.Fatalf("status=%s err=%v, want failed", status, err)
	}
	if got := e.load(t, string(id)).FailureReason(); got != "insufficient_funds" {
		t.Errorf("failure_reason = %q", got)
	}
}

// O PSP não conhece a tentativa. Antes da carência NÃO se conclui nada (pode estar em fila);
// depois dela, conclui-se que nunca chegou.
func TestResolveStuck_NotFound_WaitsForGracePeriodThenFails(t *testing.T) {
	e := setup(t)
	id := unknownPayment(t, e, modeTimeoutLost)

	status, err := e.resolve.Execute(e.ctx, id)
	if err != nil || status != payment.StatusUnknown {
		t.Fatalf("dentro da carência: status=%s err=%v, want unknown", status, err)
	}

	e.advanceClock(testGrace - time.Second)
	if status, _ := e.resolve.Execute(e.ctx, id); status != payment.StatusUnknown {
		t.Fatalf("ainda dentro da carência (1s antes): status=%s", status)
	}

	e.advanceClock(2 * time.Second)
	status, err = e.resolve.Execute(e.ctx, id)
	if err != nil || status != payment.StatusFailed {
		t.Fatalf("depois da carência: status=%s err=%v, want failed", status, err)
	}
	if got := e.load(t, string(id)).FailureReason(); got != usecase.ReasonPSPNeverReceived {
		t.Errorf("failure_reason = %q, want %q", got, usecase.ReasonPSPNeverReceived)
	}
}

// Aprovação que aparece só depois da carência ainda vence: o Lookup manda no desfecho.
func TestResolveStuck_LateApproval_StillWinsOverGracePeriod(t *testing.T) {
	e := setup(t)
	id := unknownPayment(t, e, modeTimeoutHappened)
	e.advanceClock(time.Hour)

	if status, err := e.resolve.Execute(e.ctx, id); err != nil || status != payment.StatusAuthorized {
		t.Fatalf("status=%s err=%v", status, err)
	}
}

func TestResolveStuck_PSPDown_KeepsUnknownAndReportsTheError(t *testing.T) {
	e := setup(t)
	id := unknownPayment(t, e, modeTimeoutHappened)
	e.psp.lookupErr = errors.New("PSP fora do ar")

	status, err := e.resolve.Execute(e.ctx, id)
	if err == nil || status != payment.StatusUnknown {
		t.Fatalf("status=%s err=%v", status, err)
	}
	if got := e.load(t, string(id)).Status(); got != payment.StatusUnknown {
		t.Errorf("uma consulta que falhou não pode mexer no estado: %s", got)
	}
}

func TestResolveStuck_OnlyActsOnStuckPayments(t *testing.T) {
	e := setup(t)
	id := e.created(t, "k1", 5000) // authorized
	before := e.psp.lookups()

	status, err := e.resolve.Execute(e.ctx, payment.ID(id))
	if err != nil || status != payment.StatusAuthorized {
		t.Fatalf("status=%s err=%v", status, err)
	}
	if e.psp.lookups() != before {
		t.Error("pagamento que não é unknown não deve gerar consulta ao PSP")
	}
	if _, err := e.resolve.Execute(e.ctx, "nao_existe"); !errors.Is(err, payment.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// 12 reconciliadores rodando ao mesmo tempo sobre o mesmo pagamento: todos terminam com o
// mesmo desfecho e a transição acontece uma única vez (lock otimista).
func TestResolveStuck_ConcurrentReconcilers_AgreeOnTheOutcome(t *testing.T) {
	e := setup(t)
	id := unknownPayment(t, e, modeTimeoutHappened)

	var wg sync.WaitGroup
	var bad atomic.Int64
	start := make(chan struct{})
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			status, err := e.resolve.Execute(e.ctx, id)
			if err != nil || status != payment.StatusAuthorized {
				t.Errorf("status=%s err=%v", status, err)
				bad.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if p := e.load(t, string(id)); p.Status() != payment.StatusAuthorized || p.Version() != 2 {
		// versão 0 (created) -> 1 (unknown, na criação) -> 2 (authorized): UMA transição a mais.
		t.Errorf("status=%s version=%d, want authorized e 2", p.Status(), p.Version())
	}
}

// barrierRepo segura os leitores até N terem lido, forçando DETERMINISTICAMENTE a corrida
// "dois reconciliadores leem unknown antes de qualquer um gravar". Sem isso o conflito de
// versão depende de sorte de agendamento (lacuna achada por mutação: o teste com 12
// goroutines rápidas nunca produzia o conflito).
type barrierRepo struct {
	payment.Repository
	n       int32
	arrived atomic.Int32
	ready   chan struct{}
}

func newBarrierRepo(r payment.Repository, n int32) *barrierRepo {
	return &barrierRepo{Repository: r, n: n, ready: make(chan struct{})}
}

func (b *barrierRepo) GetByID(ctx context.Context, id payment.ID) (*payment.Payment, error) {
	p, err := b.Repository.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if b.arrived.Add(1) == b.n {
		close(b.ready)
	}
	select {
	case <-b.ready:
	case <-time.After(3 * time.Second):
		return nil, errors.New("barreira: outro leitor não chegou")
	}
	return p, nil
}

func TestResolveStuck_LosingTheOptimisticLockRace_ReturnsTheWinnersOutcome(t *testing.T) {
	e := setup(t)
	id := unknownPayment(t, e, modeTimeoutHappened)

	resolver := usecase.NewResolveStuck(postgres.NewTxManager(e.pool), newBarrierRepo(e.payments, 2), e.psp, e.events(), testGrace,
		func() time.Time { return time.Unix(0, e.clock.Load()).UTC() })

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, err := resolver.Execute(e.ctx, id)
			if err != nil || status != payment.StatusAuthorized {
				t.Errorf("status=%s err=%v: o perdedor da corrida deve devolver o desfecho do vencedor", status, err)
			}
		}()
	}
	wg.Wait()

	if p := e.load(t, string(id)); p.Status() != payment.StatusAuthorized || p.Version() != 2 {
		t.Errorf("status=%s version=%d, want authorized e 2 (uma única transição)", p.Status(), p.Version())
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

// abandoned grava um pagamento created sem desfecho: o processo caiu depois do INSERT e o
// cliente nunca repetiu a requisição (não há nada no PSP a menos que o teste o ponha).
func abandoned(t *testing.T, e *env, id string) payment.ID {
	t.Helper()
	p, err := payment.New(payment.ID(id), payment.MerchantID(e.merchant), brl(t, 5000), t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.payments.Insert(e.ctx, p, "k-"+id); err != nil {
		t.Fatal(err)
	}
	return p.ID()
}

// O processo caiu DEPOIS de o PSP aprovar e ANTES de gravarmos. Sem reconciliação, ficaria
// created para sempre com dinheiro autorizado no PSP.
func TestResolveStuck_AbandonedCreated_PSPHadApproved_BecomesAuthorized(t *testing.T) {
	e := setup(t)
	id := abandoned(t, e, "pay_ab1")
	e.psp.auths[string(id)] = "auth_9"

	status, err := e.resolve.Execute(e.ctx, id)
	if err != nil || status != payment.StatusAuthorized {
		t.Fatalf("status=%s err=%v, want authorized", status, err)
	}
	if got := e.load(t, string(id)).PSPReference(); got != "auth_9" {
		t.Errorf("psp_reference = %q", got)
	}
	if got := e.eventTypes(t); len(got) != 1 || got[0] != "payment.authorized" {
		t.Errorf("eventos = %v, want [payment.authorized]", got)
	}
}

// O PSP nunca recebeu: só depois da carência o pagamento abandonado vira failed.
func TestResolveStuck_AbandonedCreated_NeverReachedPSP_FailsOnlyAfterGrace(t *testing.T) {
	e := setup(t)
	id := abandoned(t, e, "pay_ab2")

	e.advanceClock(testGrace - time.Second)
	if status, err := e.resolve.Execute(e.ctx, id); err != nil || status != payment.StatusCreated {
		t.Fatalf("dentro da carência: status=%s err=%v, want created", status, err)
	}
	e.advanceClock(2 * time.Second)
	if status, err := e.resolve.Execute(e.ctx, id); err != nil || status != payment.StatusFailed {
		t.Fatalf("depois da carência: status=%s err=%v, want failed", status, err)
	}
	if got := e.load(t, string(id)).FailureReason(); got != usecase.ReasonPSPNeverReceived {
		t.Errorf("failure_reason = %q", got)
	}
}

// Depois de a reconciliação resolver, o cliente que finalmente repete a requisição recebe o
// desfecho real (a chave estava parada em payment_created; o create só avança o ponto).
func TestResolveStuck_ThenClientRetry_GetsTheReconciledOutcome(t *testing.T) {
	e := setup(t)
	keys := &faultyKeys{Store: e.keys.Store}
	keys.failAdvanceN.Store(2) // cai ao avançar para psp_resolved, depois de o PSP aprovar
	create := usecase.NewCreatePayment(postgres.NewTxManager(e.pool), e.payments, keys, e.psp, e.events(),
		func() payment.ID { return "pay_retry" }, func() time.Time { return t0 })

	if _, err := create.Execute(e.ctx, e.in("retry-key", 5000)); err == nil {
		t.Fatal("esperava a falha injetada")
	}
	// A transação da fase 2 desfez: o PSP autorizou, mas aqui o pagamento segue created.
	if got := e.load(t, "pay_retry").Status(); got != payment.StatusCreated {
		t.Fatalf("preparação: status = %s, want created", got)
	}
	if status, err := e.resolve.Execute(e.ctx, "pay_retry"); err != nil || status != payment.StatusAuthorized {
		t.Fatalf("status=%s err=%v, want authorized", status, err)
	}

	e.expireLease(t, "retry-key")
	out, err := create.Execute(e.ctx, e.in("retry-key", 5000))
	if err != nil {
		t.Fatal(err)
	}
	if out.Payment.Status != "authorized" {
		t.Errorf("status no retry = %q, want authorized", out.Payment.Status)
	}
	if e.count(t, "payments") != 1 {
		t.Errorf("payments = %d, want 1", e.count(t, "payments"))
	}
	if _, _, auths, _ := e.psp.stats(); auths != 1 {
		t.Errorf("autorizações no PSP = %d, want 1", auths)
	}
}
