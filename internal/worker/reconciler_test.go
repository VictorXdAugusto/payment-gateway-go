package worker_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/worker"
)

type fakeLister struct {
	mu      sync.Mutex
	ids     []payment.ID // ordenados
	cutoffs []time.Time
	err     error
}

func (f *fakeLister) ListStuck(_ context.Context, before time.Time, after payment.ID, limit int) ([]payment.ID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cutoffs = append(f.cutoffs, before)
	if f.err != nil {
		return nil, f.err
	}
	var out []payment.ID
	for _, id := range f.ids {
		if id > after && len(out) < limit {
			out = append(out, id)
		}
	}
	return out, nil
}

type fakeResolver struct {
	mu      sync.Mutex
	results map[payment.ID]payment.Status
	errs    map[payment.ID]error
	calls   []payment.ID
}

func (f *fakeResolver) Execute(_ context.Context, id payment.ID) (payment.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id)
	if err := f.errs[id]; err != nil {
		return payment.StatusUnknown, err
	}
	if s, ok := f.results[id]; ok {
		return s, nil
	}
	return payment.StatusAuthorized, nil
}

var reconNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func newRecon(l *fakeLister, r *fakeResolver, cfg worker.ReconcilerConfig) *worker.Reconciler {
	return worker.NewReconciler(l, r, cfg, quiet(), func() time.Time { return reconNow })
}

func TestReconciler_WalksEveryPage_AndCountsOutcomes(t *testing.T) {
	l := &fakeLister{ids: []payment.ID{"p1", "p2", "p3", "p4", "p5"}}
	r := &fakeResolver{results: map[payment.ID]payment.Status{
		"p2": payment.StatusFailed, "p3": payment.StatusUnknown, "p4": payment.StatusCreated,
	}}
	rec := newRecon(l, r, worker.ReconcilerConfig{StaleAfter: time.Minute, PageSize: 2, MaxConsecutiveErrors: 3})

	rep, err := rec.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Examined != 5 || rep.Resolved != 3 || rep.Pending != 2 || rep.Errors != 0 || rep.Aborted {
		t.Errorf("report = %+v, want 5 examinados, 3 resolvidos, 2 pendentes", rep)
	}
	if len(r.calls) != 5 {
		t.Errorf("chamadas = %v: cada pagamento exatamente uma vez", r.calls)
	}
}

// O corte de "parado há tempo suficiente" sai do relógio injetado menos StaleAfter.
func TestReconciler_OnlyAsksForPaymentsOlderThanStaleAfter(t *testing.T) {
	l := &fakeLister{ids: []payment.ID{"p1"}}
	rec := newRecon(l, &fakeResolver{}, worker.ReconcilerConfig{StaleAfter: 90 * time.Second, PageSize: 10, MaxConsecutiveErrors: 1})
	if _, err := rec.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := reconNow.Add(-90 * time.Second); !l.cutoffs[0].Equal(want) {
		t.Errorf("corte = %v, want %v", l.cutoffs[0], want)
	}
}

// PSP fora do ar: depois de N falhas seguidas a varredura PARA em vez de martelar o serviço.
func TestReconciler_AbortsAfterConsecutiveErrors(t *testing.T) {
	l := &fakeLister{ids: []payment.ID{"p1", "p2", "p3", "p4", "p5"}}
	down := errors.New("psp fora do ar")
	r := &fakeResolver{errs: map[payment.ID]error{"p1": down, "p2": down, "p3": down, "p4": down, "p5": down}}
	rec := newRecon(l, r, worker.ReconcilerConfig{StaleAfter: time.Minute, PageSize: 10, MaxConsecutiveErrors: 3})

	rep, err := rec.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Aborted || rep.Errors != 3 || len(r.calls) != 3 {
		t.Errorf("report = %+v calls=%v, want abortado na 3ª falha seguida", rep, r.calls)
	}
}

// Sucesso entre as falhas zera a contagem: falhas isoladas não derrubam a varredura.
func TestReconciler_AnIsolatedErrorDoesNotAbort(t *testing.T) {
	l := &fakeLister{ids: []payment.ID{"p1", "p2", "p3", "p4", "p5"}}
	boom := errors.New("boom")
	r := &fakeResolver{errs: map[payment.ID]error{"p1": boom, "p2": boom, "p4": boom}}
	rec := newRecon(l, r, worker.ReconcilerConfig{StaleAfter: time.Minute, PageSize: 10, MaxConsecutiveErrors: 3})

	rep, err := rec.RunOnce(context.Background())
	if err != nil || rep.Aborted || rep.Errors != 3 || rep.Resolved != 2 {
		t.Errorf("report = %+v err=%v", rep, err)
	}
}

func TestReconciler_ListingErrorIsReturned(t *testing.T) {
	want := errors.New("banco fora")
	rec := newRecon(&fakeLister{err: want}, &fakeResolver{}, worker.ReconcilerConfig{StaleAfter: time.Minute, PageSize: 10, MaxConsecutiveErrors: 1})
	if _, err := rec.RunOnce(context.Background()); !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
}

func TestReconciler_StopsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	l := &fakeLister{ids: []payment.ID{"p1", "p2", "p3"}}
	r := &cancellingResolver{cancel: cancel}
	rec := worker.NewReconciler(l, r, worker.ReconcilerConfig{StaleAfter: time.Minute, PageSize: 10, MaxConsecutiveErrors: 3}, quiet(),
		func() time.Time { return reconNow })

	rep, err := rec.RunOnce(ctx)
	if err != nil || r.calls != 1 || rep.Examined != 1 {
		t.Errorf("calls=%d rep=%+v err=%v: deve parar logo depois do cancelamento", r.calls, rep, err)
	}
}

type cancellingResolver struct {
	cancel func()
	calls  int
}

func (c *cancellingResolver) Execute(context.Context, payment.ID) (payment.Status, error) {
	c.calls++
	c.cancel()
	return payment.StatusAuthorized, nil
}

func TestReconciler_Run_LoopsUntilCancelled(t *testing.T) {
	l := &fakeLister{ids: []payment.ID{"p1"}}
	r := &fakeResolver{}
	rec := newRecon(l, r, worker.ReconcilerConfig{Interval: 5 * time.Millisecond, StaleAfter: time.Minute, PageSize: 10, MaxConsecutiveErrors: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { rec.Run(ctx); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run não terminou depois do cancelamento")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) < 2 {
		t.Errorf("Run devia varrer mais de uma vez, chamadas = %d", len(r.calls))
	}
}

// "Continua preso" (PSP respondeu, mas ainda não sabe) é uma consulta BEM-SUCEDIDA: zera a
// contagem de falhas seguidas, senão um PSP saudável seria tratado como fora do ar.
func TestReconciler_StillPendingCountsAsAHealthyAnswer(t *testing.T) {
	l := &fakeLister{ids: []payment.ID{"p1", "p2", "p3"}}
	boom := errors.New("boom")
	r := &fakeResolver{
		errs:    map[payment.ID]error{"p1": boom, "p3": boom},
		results: map[payment.ID]payment.Status{"p2": payment.StatusUnknown},
	}
	rec := newRecon(l, r, worker.ReconcilerConfig{StaleAfter: time.Minute, PageSize: 10, MaxConsecutiveErrors: 2})

	rep, err := rec.RunOnce(context.Background())
	if err != nil || rep.Aborted || rep.Examined != 3 {
		t.Errorf("report = %+v err=%v, want 3 examinados sem abortar", rep, err)
	}
}
