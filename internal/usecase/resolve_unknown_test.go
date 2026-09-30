package usecase_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
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
func TestResolveUnknown_PSPHadApproved_BecomesAuthorized(t *testing.T) {
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

func TestResolveUnknown_PSPHadDeclined_BecomesFailed(t *testing.T) {
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
func TestResolveUnknown_NotFound_WaitsForGracePeriodThenFails(t *testing.T) {
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
func TestResolveUnknown_LateApproval_StillWinsOverGracePeriod(t *testing.T) {
	e := setup(t)
	id := unknownPayment(t, e, modeTimeoutHappened)
	e.advanceClock(time.Hour)

	if status, err := e.resolve.Execute(e.ctx, id); err != nil || status != payment.StatusAuthorized {
		t.Fatalf("status=%s err=%v", status, err)
	}
}

func TestResolveUnknown_PSPDown_KeepsUnknownAndReportsTheError(t *testing.T) {
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

func TestResolveUnknown_OnlyActsOnUnknownPayments(t *testing.T) {
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
func TestResolveUnknown_ConcurrentReconcilers_AgreeOnTheOutcome(t *testing.T) {
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

func TestResolveUnknown_LosingTheOptimisticLockRace_ReturnsTheWinnersOutcome(t *testing.T) {
	e := setup(t)
	id := unknownPayment(t, e, modeTimeoutHappened)

	resolver := usecase.NewResolveUnknown(newBarrierRepo(e.payments, 2), e.psp, testGrace,
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
