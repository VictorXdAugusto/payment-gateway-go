package postgres_test

import (
	"testing"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres"
)

func TestStats_Backlog_CountsOnlyWhatIsWaiting(t *testing.T) {
	e := setup(t)
	e.addEvents(t, "e1", "e2", "e3", "e4")
	for id, status := range map[string]string{"e3": "dead", "e4": "delivered"} {
		if _, err := e.pool.Exec(e.ctx, `UPDATE outbox_events SET status = $2,
			delivered_at = CASE WHEN $2 = 'delivered' THEN now() END WHERE event_id = $1`, id, status); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE outbox_events SET next_attempt_at = now() - interval '5 minutes' WHERE event_id = 'e1'`); err != nil {
		t.Fatal(err)
	}
	e.insertAt(t, "pay_u", "unknown", now)
	e.insertAt(t, "pay_c", "created", now)
	e.insertAt(t, "pay_ok", "authorized", now)

	b, err := postgres.NewStatsRepository(e.txm).Backlog(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if b.OutboxPending != 2 || b.OutboxDead != 1 || b.PaymentsStuck != 2 {
		t.Errorf("backlog = %+v, want pending 2, dead 1, stuck 2", b)
	}
	if b.OutboxOldestPending < 5*time.Minute || b.OutboxOldestPending > 6*time.Minute {
		t.Errorf("idade da mais antiga = %v, want ~5m", b.OutboxOldestPending)
	}
}

func TestStats_Backlog_EmptyDatabaseIsAllZeros(t *testing.T) {
	e := setup(t)
	b, err := postgres.NewStatsRepository(e.txm).Backlog(e.ctx)
	if err != nil || b.OutboxPending != 0 || b.OutboxOldestPending != 0 || b.PaymentsStuck != 0 {
		t.Errorf("backlog = %+v err=%v", b, err)
	}
}
