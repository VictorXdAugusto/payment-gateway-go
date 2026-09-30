package worker_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/worker"
)

type tally struct {
	mu sync.Mutex
	m  map[string]int
}

func newTally() *tally { return &tally{m: map[string]int{}} }
func (t *tally) add(k string) {
	t.mu.Lock()
	t.m[k]++
	t.mu.Unlock()
}
func (t *tally) get(k string) int { t.mu.Lock(); defer t.mu.Unlock(); return t.m[k] }

// O dispatcher reporta UM desfecho por entrega, com o nome certo para cada caminho.
func TestDispatcher_ReportsEachOutcome(t *testing.T) {
	e := setup(t)
	rc := newReceiver(t)
	e.webhook(t, rc.srv.URL)
	got := newTally()
	c := cfg()
	c.OnOutcome = got.add

	e.add(t, 2)
	d := worker.NewDispatcher(e.repo, realSender(true), c, quiet())
	if _, err := d.RunOnce(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got.get("delivered") != 2 {
		t.Errorf("delivered = %d, want 2", got.get("delivered"))
	}

	rc.status.Store(500)
	e.add(t, 1)
	if _, err := d.RunOnce(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got.get("rescheduled") != 1 {
		t.Errorf("rescheduled = %d, want 1", got.get("rescheduled"))
	}
}

func TestDispatcher_ReportsSkippedDeadAndLostClaim(t *testing.T) {
	e := setup(t)
	got := newTally()
	c := cfg()
	c.OnOutcome = got.add

	e.add(t, 1) // lojista sem webhook
	if _, err := worker.NewDispatcher(e.repo, realSender(true), c, quiet()).RunOnce(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got.get("skipped") != 1 {
		t.Errorf("skipped = %d", got.get("skipped"))
	}

	e.webhook(t, "http://10.0.0.1/hook") // destino proibido: falha permanente
	e.add(t, 1)
	if _, err := worker.NewDispatcher(e.repo, realSender(false), c, quiet()).RunOnce(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got.get("dead") != 1 {
		t.Errorf("dead = %d", got.get("dead"))
	}
}

func TestDispatcher_ReportsALostClaim(t *testing.T) {
	e := setup(t)
	e.webhook(t, "https://loja.example/hook")
	e.add(t, 1)
	slow := &blockingSender{release: make(chan struct{}), entered: make(chan struct{})}
	got := newTally()
	c := cfg()
	c.OnOutcome = got.add

	a := worker.NewDispatcher(e.repo, slow, c, quiet())
	done := make(chan struct{})
	go func() { defer close(done); _, _ = a.RunOnce(e.ctx) }()
	<-slow.entered
	if _, err := e.pool.Exec(e.ctx, `UPDATE outbox_events SET locked_until = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.NewDispatcher(e.repo, slow, cfg(), quiet()).RunOnce(e.ctx); err != nil {
		t.Fatal(err)
	}
	close(slow.release)
	<-done
	if got.get("claim_lost") != 1 {
		t.Errorf("claim_lost = %d, want 1", got.get("claim_lost"))
	}
}

func TestReconciler_Hooks_ReportResultsAndSweeps(t *testing.T) {
	l := &fakeLister{ids: []payment.ID{"p1", "p2", "p3"}}
	r := &fakeResolver{
		results: map[payment.ID]payment.Status{"p2": payment.StatusUnknown},
		errs:    map[payment.ID]error{"p3": context.DeadlineExceeded},
	}
	results, aborted := newTally(), newTally()
	rec := worker.NewReconciler(l, r, worker.ReconcilerConfig{
		StaleAfter: time.Minute, PageSize: 10, MaxConsecutiveErrors: 5,
		OnResult: results.add,
		OnSweep: func(a bool) {
			if a {
				aborted.add("true")
			} else {
				aborted.add("false")
			}
		},
	}, quiet(), func() time.Time { return reconNow })

	if _, err := rec.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if results.get("resolved") != 1 || results.get("pending") != 1 || results.get("error") != 1 {
		t.Errorf("resultados = %v", results.m)
	}
	if aborted.get("false") != 1 || aborted.get("true") != 0 {
		t.Errorf("varreduras = %v", aborted.m)
	}
}

func TestHousekeeper_Hook_ReportsWhatEachBatchDeleted(t *testing.T) {
	purge, _ := scripted(10, 3)
	got := newTally()
	h := worker.NewHousekeeper([]worker.Retention{{Name: "keys", OlderThan: time.Hour, Purger: purge}},
		worker.HousekeeperConfig{Batch: 10, OnPurged: func(p string, n int64) {
			for i := int64(0); i < n; i++ {
				got.add(p)
			}
		}}, quiet())
	h.RunOnce(context.Background())
	if got.get("keys") != 13 {
		t.Errorf("reportado = %d, want 13", got.get("keys"))
	}
}

// Varredura que falhou ao listar não é "varredura concluída": não entra na métrica.
func TestReconciler_Hook_DoesNotReportASweepThatFailedToList(t *testing.T) {
	calls := 0
	rec := worker.NewReconciler(&fakeLister{err: context.DeadlineExceeded}, &fakeResolver{},
		worker.ReconcilerConfig{StaleAfter: time.Minute, PageSize: 10, MaxConsecutiveErrors: 1, OnSweep: func(bool) { calls++ }},
		quiet(), func() time.Time { return reconNow })
	if _, err := rec.RunOnce(context.Background()); err == nil {
		t.Fatal("esperava o erro de listagem")
	}
	if calls != 0 {
		t.Errorf("OnSweep chamado %d vezes numa varredura que falhou", calls)
	}
}

func TestHousekeeper_Hook_IsNotCalledWhenNothingWasDeleted(t *testing.T) {
	purge, _ := scripted(0)
	calls := 0
	worker.NewHousekeeper([]worker.Retention{{Name: "keys", OlderThan: time.Hour, Purger: purge}},
		worker.HousekeeperConfig{Batch: 10, OnPurged: func(string, int64) { calls++ }}, quiet()).RunOnce(context.Background())
	if calls != 0 {
		t.Errorf("OnPurged chamado %d vezes sem nada apagado", calls)
	}
}
