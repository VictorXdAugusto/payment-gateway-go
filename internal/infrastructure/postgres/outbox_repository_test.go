package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/outbox"
)

func (e *env) outbox() *postgres.OutboxRepository { return postgres.NewOutboxRepository(e.txm) }

func (e *env) event(id string) outbox.Event {
	return outbox.Event{EventID: id, MerchantID: e.merchant, Type: "payment.captured", PaymentID: "pay_1",
		Payload: []byte(`{"id":"` + id + `"}`), CreatedAt: now}
}

func (e *env) addEvents(t testing.TB, ids ...string) {
	t.Helper()
	evs := make([]outbox.Event, len(ids))
	for i, id := range ids {
		evs[i] = e.event(id)
	}
	if err := e.outbox().Add(e.ctx, evs...); err != nil {
		t.Fatal(err)
	}
}

func (e *env) setWebhook(t testing.TB, url, secret string) {
	t.Helper()
	if _, err := e.pool.Exec(e.ctx, `UPDATE merchants SET webhook_url = $1, webhook_secret = $2 WHERE id = $3::uuid`,
		url, secret, e.merchant); err != nil {
		t.Fatal(err)
	}
}

func (e *env) eventState(t testing.TB, id string) (status string, attempts int, lastErr string) {
	t.Helper()
	err := e.pool.QueryRow(e.ctx, `SELECT status, attempts, last_error FROM outbox_events WHERE event_id = $1`, id).
		Scan(&status, &attempts, &lastErr)
	if err != nil {
		t.Fatal(err)
	}
	return
}

// expireLease simula workers que morreram: os leases passam a estar no passado.
func (e *env) expireLeases(t testing.TB) {
	t.Helper()
	if _, err := e.pool.Exec(e.ctx, `UPDATE outbox_events SET locked_until = now() - interval '1 hour' WHERE locked_until IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
}

func ids(ds []outbox.Delivery) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Event.EventID
	}
	return out
}

func TestOutbox_Claim_ReturnsDueEventsWithMerchantWebhook(t *testing.T) {
	e := setup(t)
	e.setWebhook(t, "https://loja.example/hook", "whsec_1")
	e.addEvents(t, "evt_1", "evt_2")

	got, err := e.outbox().Claim(e.ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Event.EventID != "evt_1" || got[1].Event.EventID != "evt_2" {
		t.Fatalf("claimed = %v (ordem de criação)", ids(got))
	}
	d := got[0]
	if d.Attempt != 1 || d.WebhookURL != "https://loja.example/hook" || d.Secret != "whsec_1" ||
		string(d.Event.Payload) != `{"id":"evt_1"}` || d.Event.MerchantID != e.merchant {
		t.Errorf("delivery = %+v", d)
	}

	// Reivindicadas: ninguém mais as enxerga enquanto o lease valer.
	if again, _ := e.outbox().Claim(e.ctx, 10, time.Minute); len(again) != 0 {
		t.Errorf("segunda reivindicação pegou %v durante o lease", ids(again))
	}
}

func TestOutbox_Claim_RespectsLimitAndDueDate(t *testing.T) {
	e := setup(t)
	e.addEvents(t, "evt_1", "evt_2", "evt_3")
	if _, err := e.pool.Exec(e.ctx, `UPDATE outbox_events SET next_attempt_at = now() + interval '1 hour' WHERE event_id = 'evt_3'`); err != nil {
		t.Fatal(err)
	}

	first, _ := e.outbox().Claim(e.ctx, 1, time.Minute)
	second, _ := e.outbox().Claim(e.ctx, 10, time.Minute)
	if len(first) != 1 || len(second) != 1 || second[0].Event.EventID == "evt_3" {
		t.Errorf("first=%v second=%v: evt_3 ainda não venceu e não pode ser reivindicado", ids(first), ids(second))
	}
}

func TestOutbox_Claim_ReturnsEmptyWebhookWhenMerchantHasNone(t *testing.T) {
	e := setup(t)
	e.addEvents(t, "evt_1")
	got, err := e.outbox().Claim(e.ctx, 10, time.Minute)
	if err != nil || len(got) != 1 || got[0].WebhookURL != "" || got[0].Secret != "" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

// Um worker que morre deixa o lease vencer; outro reassume e a tentativa conta.
func TestOutbox_Claim_ReassumesAfterLeaseExpiry_CountingTheAttempt(t *testing.T) {
	e := setup(t)
	e.addEvents(t, "evt_1")
	first, _ := e.outbox().Claim(e.ctx, 10, time.Minute)

	e.expireLeases(t)
	second, err := e.outbox().Claim(e.ctx, 10, time.Minute)
	if err != nil || len(second) != 1 {
		t.Fatalf("second=%v err=%v", second, err)
	}
	if first[0].Attempt != 1 || second[0].Attempt != 2 {
		t.Errorf("tentativas: %d e %d, want 1 e 2 (uma queda no meio conta como tentativa)", first[0].Attempt, second[0].Attempt)
	}
}

// Fencing: o resultado de um worker cujo lease venceu não pode sobrescrever o do novo dono.
func TestOutbox_Results_AreFencedByTheClaim(t *testing.T) {
	e := setup(t)
	e.addEvents(t, "evt_1")
	stale, _ := e.outbox().Claim(e.ctx, 1, time.Minute)
	e.expireLeases(t)
	current, _ := e.outbox().Claim(e.ctx, 1, time.Minute)

	// O worker antigo acorda e tenta gravar QUALQUER resultado: todos descartados.
	repo := e.outbox()
	if err := repo.MarkDelivered(e.ctx, stale[0]); !errors.Is(err, outbox.ErrClaimLost) {
		t.Errorf("MarkDelivered do antigo: %v", err)
	}
	if err := repo.Reschedule(e.ctx, stale[0], time.Hour, "x"); !errors.Is(err, outbox.ErrClaimLost) {
		t.Errorf("Reschedule do antigo: %v", err)
	}
	if err := repo.MarkDead(e.ctx, stale[0], "x"); !errors.Is(err, outbox.ErrClaimLost) {
		t.Errorf("MarkDead do antigo: %v", err)
	}
	if err := repo.MarkSkipped(e.ctx, stale[0], "x"); !errors.Is(err, outbox.ErrClaimLost) {
		t.Errorf("MarkSkipped do antigo: %v", err)
	}
	if status, _, _ := e.eventState(t, "evt_1"); status != "pending" {
		t.Fatalf("status = %s: o worker antigo alterou a entrega", status)
	}

	if err := repo.MarkDelivered(e.ctx, current[0]); err != nil {
		t.Fatalf("o dono atual deveria conseguir: %v", err)
	}
	// Idempotência do resultado: gravar de novo depois de concluída também é descartado.
	if err := repo.MarkDelivered(e.ctx, current[0]); !errors.Is(err, outbox.ErrClaimLost) {
		t.Errorf("segunda gravação: %v", err)
	}
}

func TestOutbox_FinalStates(t *testing.T) {
	e := setup(t)
	e.addEvents(t, "evt_ok", "evt_dead", "evt_skip", "evt_retry")
	ds, _ := e.outbox().Claim(e.ctx, 10, time.Minute)
	byID := map[string]outbox.Delivery{}
	for _, d := range ds {
		byID[d.Event.EventID] = d
	}
	repo := e.outbox()

	if err := repo.MarkDelivered(e.ctx, byID["evt_ok"]); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkDead(e.ctx, byID["evt_dead"], "500 sempre"); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkSkipped(e.ctx, byID["evt_skip"], "sem webhook"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Reschedule(e.ctx, byID["evt_retry"], 10*time.Minute, "timeout"); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{"evt_ok": "delivered", "evt_dead": "dead", "evt_skip": "skipped", "evt_retry": "pending"}
	for id, st := range want {
		if status, _, _ := e.eventState(t, id); status != st {
			t.Errorf("%s: status = %s, want %s", id, status, st)
		}
	}
	if _, _, msg := e.eventState(t, "evt_dead"); msg != "500 sempre" {
		t.Errorf("last_error = %q", msg)
	}

	// Reagendada para daqui a 10 min: não é reivindicável agora, mesmo com o lease liberado.
	if again, _ := repo.Claim(e.ctx, 10, time.Minute); len(again) != 0 {
		t.Errorf("reivindicou %v; nada deveria estar devido", ids(again))
	}
	var delivered bool
	if err := e.pool.QueryRow(e.ctx, `SELECT delivered_at IS NOT NULL FROM outbox_events WHERE event_id = 'evt_ok'`).Scan(&delivered); err != nil || !delivered {
		t.Errorf("delivered_at não gravado (%v)", err)
	}
}

// O coração do padrão: o evento entra ou desfaz JUNTO com o restante da transação.
func TestOutbox_Add_JoinsTheCallersTransaction(t *testing.T) {
	e := setup(t)
	boom := errors.New("falha depois de gravar o evento")

	err := e.txm.WithinTx(e.ctx, func(ctx context.Context) error {
		if err := e.outbox().Add(ctx, e.event("evt_1")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || e.count(t, "outbox_events") != 0 {
		t.Fatalf("err=%v eventos=%d: o evento sobreviveu ao rollback", err, e.count(t, "outbox_events"))
	}

	err = e.txm.WithinTx(e.ctx, func(ctx context.Context) error { return e.outbox().Add(ctx, e.event("evt_2")) })
	if err != nil || e.count(t, "outbox_events") != 1 {
		t.Fatalf("err=%v eventos=%d", err, e.count(t, "outbox_events"))
	}
}

func TestOutbox_Add_RejectsDuplicateEventID(t *testing.T) {
	e := setup(t)
	e.addEvents(t, "evt_1")
	if err := e.outbox().Add(e.ctx, e.event("evt_1")); err == nil {
		t.Fatal("event_id repetido deveria ser rejeitado (é a chave de dedupe do lojista)")
	}
}

// SKIP LOCKED: 6 workers esvaziando a fila ao mesmo tempo nunca pegam a mesma entrega.
func TestOutbox_Claim_ConcurrentWorkersGetDisjointBatches(t *testing.T) {
	e := setup(t)
	const total = 300
	all := make([]string, total)
	for i := range all {
		all[i] = fmt.Sprintf("evt_%03d", i)
	}
	e.addEvents(t, all...)

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				batch, err := e.outbox().Claim(e.ctx, 7, time.Minute)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if len(batch) == 0 {
					return
				}
				mu.Lock()
				for _, d := range batch {
					seen[d.Event.EventID]++
				}
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("reivindicadas %d de %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s reivindicado %d vezes", id, n)
		}
	}
}
