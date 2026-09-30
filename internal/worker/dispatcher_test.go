package worker_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres/pgtest"
	infra "github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/webhook"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/outbox"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/webhook"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/worker"
)

const secret = "whsec_test"

type env struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	repo     *postgres.OutboxRepository
	merchant string
	seq      atomic.Int64
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := pgtest.New(t)
	ctx := context.Background()
	return &env{
		ctx: ctx, pool: pool,
		repo:     postgres.NewOutboxRepository(postgres.NewTxManager(pool)),
		merchant: pgtest.Merchant(ctx, t, pool, "loja"),
	}
}

func (e *env) webhook(t testing.TB, url string) {
	t.Helper()
	if _, err := e.pool.Exec(e.ctx, `UPDATE merchants SET webhook_url = $1, webhook_secret = $2 WHERE id = $3::uuid`,
		url, secret, e.merchant); err != nil {
		t.Fatal(err)
	}
}

func (e *env) add(t testing.TB, n int) []string {
	t.Helper()
	var evs []outbox.Event
	var ids []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("evt_%04d", e.seq.Add(1))
		ids = append(ids, id)
		evs = append(evs, outbox.Event{EventID: id, MerchantID: e.merchant, Type: "payment.captured", PaymentID: "pay_1",
			Payload: []byte(fmt.Sprintf(`{"id":"%s","type":"payment.captured"}`, id)), CreatedAt: time.Now()})
	}
	if err := e.repo.Add(e.ctx, evs...); err != nil {
		t.Fatal(err)
	}
	return ids
}

func (e *env) state(t testing.TB, id string) (status string, attempts int, lastErr string) {
	t.Helper()
	if err := e.pool.QueryRow(e.ctx, `SELECT status, attempts, last_error FROM outbox_events WHERE event_id = $1`, id).
		Scan(&status, &attempts, &lastErr); err != nil {
		t.Fatal(err)
	}
	return
}

// makeDue faz todas as entregas pendentes ficarem devidas agora (pula a espera do backoff).
func (e *env) makeDue(t testing.TB) {
	t.Helper()
	if _, err := e.pool.Exec(e.ctx, `UPDATE outbox_events SET next_attempt_at = now() - interval '1 second' WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func cfg() worker.Config {
	return worker.Config{BatchSize: 50, Concurrency: 8, PollInterval: 10 * time.Millisecond, Lease: time.Minute,
		MaxAttempts: 5, Backoff: worker.Backoff{Base: 10 * time.Second, Max: time.Hour}}
}

func realSender(allowPrivate bool) *infra.Sender {
	return infra.NewSender(infra.Config{Timeout: time.Second, AllowPrivate: allowPrivate})
}

// receiver é um endpoint de lojista que verifica a assinatura e conta o que chegou.
type receiver struct {
	srv     *httptest.Server
	mu      sync.Mutex
	hits    map[string]int
	badSigs atomic.Int32
	status  atomic.Int32 // 0 = 200; senão responde este código
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{hits: map[string]int{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		if err := webhook.Verify(secret, req.Header.Get(webhook.SignatureHeader), body, time.Now(), 5*time.Minute); err != nil {
			r.badSigs.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		r.mu.Lock()
		r.hits[req.Header.Get(webhook.EventIDHeader)]++
		r.mu.Unlock()
		if code := int(r.status.Load()); code != 0 {
			w.WriteHeader(code)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) count(id string) int { r.mu.Lock(); defer r.mu.Unlock(); return r.hits[id] }
func (r *receiver) distinct() int       { r.mu.Lock(); defer r.mu.Unlock(); return len(r.hits) }

func TestDispatcher_DeliversSignedEvents(t *testing.T) {
	e := setup(t)
	rc := newReceiver(t)
	e.webhook(t, rc.srv.URL)
	ids := e.add(t, 3)

	d := worker.NewDispatcher(e.repo, realSender(true), cfg(), quiet())
	if n, err := d.RunOnce(e.ctx); err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}

	for _, id := range ids {
		if st, attempts, _ := e.state(t, id); st != "delivered" || attempts != 1 || rc.count(id) != 1 {
			t.Errorf("%s: status=%s tentativas=%d recebido=%d", id, st, attempts, rc.count(id))
		}
	}
	if rc.badSigs.Load() != 0 {
		t.Errorf("%d entregas com assinatura inválida", rc.badSigs.Load())
	}
	// Nada mais a fazer.
	if n, _ := d.RunOnce(e.ctx); n != 0 {
		t.Errorf("segunda rodada processou %d", n)
	}
}

func TestDispatcher_RetriesWithBackoffUntilTheEndpointRecovers(t *testing.T) {
	e := setup(t)
	rc := newReceiver(t)
	rc.status.Store(500)
	e.webhook(t, rc.srv.URL)
	id := e.add(t, 1)[0]
	d := worker.NewDispatcher(e.repo, realSender(true), cfg(), quiet())

	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := d.RunOnce(e.ctx); err != nil {
			t.Fatal(err)
		}
		st, attempts, lastErr := e.state(t, id)
		if st != "pending" || attempts != attempt || lastErr == "" {
			t.Fatalf("depois da falha %d: status=%s tentativas=%d erro=%q", attempt, st, attempts, lastErr)
		}
		// A espera respeita o backoff (base 10s, faixa [5s,10s] na 1ª falha): ainda não venceu.
		if n, _ := d.RunOnce(e.ctx); n != 0 {
			t.Fatalf("reentregou antes do backoff vencer")
		}
		e.makeDue(t)
	}

	rc.status.Store(0) // o lojista consertou o endpoint
	if _, err := d.RunOnce(e.ctx); err != nil {
		t.Fatal(err)
	}
	if st, attempts, lastErr := e.state(t, id); st != "delivered" || attempts != 3 || lastErr != "" {
		t.Fatalf("final: status=%s tentativas=%d erro=%q", st, attempts, lastErr)
	}
}

func TestDispatcher_SchedulesTheNextAttemptInsideTheBackoffBand(t *testing.T) {
	e := setup(t)
	rc := newReceiver(t)
	rc.status.Store(500)
	e.webhook(t, rc.srv.URL)
	id := e.add(t, 1)[0]

	if _, err := worker.NewDispatcher(e.repo, realSender(true), cfg(), quiet()).RunOnce(e.ctx); err != nil {
		t.Fatal(err)
	}
	var wait float64
	if err := e.pool.QueryRow(e.ctx, `SELECT EXTRACT(EPOCH FROM (next_attempt_at - now())) FROM outbox_events WHERE event_id = $1`, id).Scan(&wait); err != nil {
		t.Fatal(err)
	}
	if wait < 4 || wait > 10.5 { // base 10s => [5s, 10s], com folga para o relógio
		t.Errorf("próxima tentativa em %.1fs, want ~[5s, 10s]", wait)
	}
}

func TestDispatcher_MovesToDeadLetterAfterMaxAttempts(t *testing.T) {
	e := setup(t)
	rc := newReceiver(t)
	rc.status.Store(503)
	e.webhook(t, rc.srv.URL)
	id := e.add(t, 1)[0]
	c := cfg()
	c.MaxAttempts = 3
	d := worker.NewDispatcher(e.repo, realSender(true), c, quiet())

	for i := 0; i < 3; i++ {
		if _, err := d.RunOnce(e.ctx); err != nil {
			t.Fatal(err)
		}
		e.makeDue(t)
	}
	st, attempts, lastErr := e.state(t, id)
	if st != "dead" || attempts != 3 || lastErr == "" {
		t.Fatalf("status=%s tentativas=%d erro=%q, want dead/3", st, attempts, lastErr)
	}
	if n, _ := d.RunOnce(e.ctx); n != 0 || rc.count(id) != 3 {
		t.Errorf("dead letter não pode ser reentregue (rodada=%d, hits=%d)", n, rc.count(id))
	}
}

// Destino proibido não gasta tentativas: repetir não vai mudar nada.
func TestDispatcher_ForbiddenDestinationGoesStraightToDeadLetter(t *testing.T) {
	e := setup(t)
	rc := newReceiver(t) // 127.0.0.1: proibido com SSRF ligado
	e.webhook(t, rc.srv.URL)
	id := e.add(t, 1)[0]

	d := worker.NewDispatcher(e.repo, realSender(false), cfg(), quiet())
	if _, err := d.RunOnce(e.ctx); err != nil {
		t.Fatal(err)
	}
	st, attempts, lastErr := e.state(t, id)
	if st != "dead" || attempts != 1 || lastErr == "" {
		t.Fatalf("status=%s tentativas=%d erro=%q, want dead/1", st, attempts, lastErr)
	}
	if rc.distinct() != 0 {
		t.Error("o servidor interno recebeu a requisição: SSRF")
	}
}

func TestDispatcher_MerchantWithoutWebhookIsSkipped(t *testing.T) {
	e := setup(t)
	id := e.add(t, 1)[0]
	if _, err := worker.NewDispatcher(e.repo, realSender(true), cfg(), quiet()).RunOnce(e.ctx); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := e.state(t, id); st != "skipped" {
		t.Errorf("status = %s", st)
	}
}

// 3 workers e 240 eventos: cada evento chega UMA vez ao lojista.
func TestDispatcher_ConcurrentWorkersDeliverEachEventOnce(t *testing.T) {
	e := setup(t)
	rc := newReceiver(t)
	e.webhook(t, rc.srv.URL)
	ids := e.add(t, 240)

	var wg sync.WaitGroup
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := worker.NewDispatcher(e.repo, realSender(true), cfg(), quiet())
			for {
				n, err := d.RunOnce(e.ctx)
				if err != nil {
					t.Errorf("RunOnce: %v", err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()

	if rc.distinct() != len(ids) {
		t.Fatalf("lojista recebeu %d eventos distintos, want %d", rc.distinct(), len(ids))
	}
	for _, id := range ids {
		if rc.count(id) != 1 {
			t.Fatalf("%s entregue %d vezes", id, rc.count(id))
		}
	}
	var pending int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM outbox_events WHERE status <> 'delivered'`).Scan(&pending)
	if pending != 0 {
		t.Errorf("%d eventos não entregues", pending)
	}
}

// blockingSender segura a primeira entrega até ser liberada, para simular um worker lento
// cujo lease vence no meio do envio.
type blockingSender struct {
	release chan struct{}
	entered chan struct{}
	calls   atomic.Int32
	failOld bool
}

func (b *blockingSender) Deliver(_ context.Context, _ outbox.Delivery) error {
	if b.calls.Add(1) == 1 {
		close(b.entered)
		<-b.release
		if b.failOld {
			return errors.New("o worker lento falhou depois")
		}
	}
	return nil
}

// At-least-once na prática: o worker A trava, o lease vence, o worker B entrega. O lojista
// pode receber DUAS vezes (por isso deduplica pelo event_id), mas o estado final é coerente
// e o resultado tardio do worker A não desfaz o do B.
func TestDispatcher_SlowWorkerLosingItsLease_DoesNotOverwriteTheNewOwner(t *testing.T) {
	for name, failOld := range map[string]bool{"A termina com sucesso": false, "A termina com falha": true} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.webhook(t, "https://loja.example/hook")
			id := e.add(t, 1)[0]
			slow := &blockingSender{release: make(chan struct{}), entered: make(chan struct{}), failOld: failOld}

			a := worker.NewDispatcher(e.repo, slow, cfg(), quiet())
			aDone := make(chan struct{})
			go func() { defer close(aDone); _, _ = a.RunOnce(e.ctx) }()
			<-slow.entered // A reivindicou (tentativa 1) e está "no meio do HTTP"

			if _, err := e.pool.Exec(e.ctx, `UPDATE outbox_events SET locked_until = now() - interval '1 hour'`); err != nil {
				t.Fatal(err)
			}
			b := worker.NewDispatcher(e.repo, slow, cfg(), quiet()) // 2ª chamada de Deliver: sucesso
			if n, err := b.RunOnce(e.ctx); err != nil || n != 1 {
				t.Fatalf("B: n=%d err=%v", n, err)
			}
			if st, attempts, _ := e.state(t, id); st != "delivered" || attempts != 2 {
				t.Fatalf("depois do B: status=%s tentativas=%d", st, attempts)
			}

			close(slow.release) // A acorda e tenta gravar o resultado dele
			<-aDone

			st, attempts, lastErr := e.state(t, id)
			if st != "delivered" || attempts != 2 || lastErr != "" {
				t.Errorf("o resultado tardio do A alterou o estado: status=%s tentativas=%d erro=%q", st, attempts, lastErr)
			}
			if slow.calls.Load() != 2 {
				t.Errorf("entregas = %d, want 2 (at-least-once: o lojista dedupe pelo event_id)", slow.calls.Load())
			}
		})
	}
}

// Shutdown gracioso: o cancelamento não abandona uma entrega em andamento.
func TestDispatcher_Run_FinishesInFlightDeliveryOnShutdown(t *testing.T) {
	e := setup(t)
	e.webhook(t, "https://loja.example/hook")
	id := e.add(t, 1)[0]
	slow := &blockingSender{release: make(chan struct{}), entered: make(chan struct{})}

	ctx, cancel := context.WithCancel(e.ctx)
	runDone := make(chan error, 1)
	d := worker.NewDispatcher(e.repo, slow, cfg(), quiet())
	go func() { runDone <- d.Run(ctx) }()

	<-slow.entered
	cancel() // SIGTERM com uma entrega no ar
	select {
	case <-runDone:
		t.Fatal("Run retornou com uma entrega ainda em andamento")
	case <-time.After(150 * time.Millisecond):
	}

	close(slow.release)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run não terminou depois da entrega")
	}
	if st, _, _ := e.state(t, id); st != "delivered" {
		t.Errorf("status = %s: o resultado da entrega em andamento tem que ser gravado", st)
	}
}

func TestDispatcher_Run_PicksUpEventsAddedWhileRunning(t *testing.T) {
	e := setup(t)
	rc := newReceiver(t)
	e.webhook(t, rc.srv.URL)

	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = worker.NewDispatcher(e.repo, realSender(true), cfg(), quiet()).Run(ctx) }()

	time.Sleep(50 * time.Millisecond) // o worker já está dormindo no poll
	id := e.add(t, 1)[0]

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, _, _ := e.state(t, id); st == "delivered" {
			cancel()
			<-done
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("o evento novo não foi entregue pelo loop de polling")
}
