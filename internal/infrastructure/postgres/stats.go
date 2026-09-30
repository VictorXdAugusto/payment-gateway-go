package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/observability"
)

// StatsRepository lê os números de backlog que alimentam os gauges. Cada consulta usa um
// índice parcial (outbox_due_idx, outbox_dead_idx, payments_stuck_idx): o custo é proporcional
// ao backlog, não ao tamanho das tabelas, então é seguro rodar a cada scrape.
type StatsRepository struct {
	tx *TxManager
}

var _ observability.BacklogSource = (*StatsRepository)(nil)

func NewStatsRepository(tx *TxManager) *StatsRepository { return &StatsRepository{tx: tx} }

func (r *StatsRepository) Backlog(ctx context.Context) (observability.Backlog, error) {
	var (
		b          observability.Backlog
		oldestSecs float64
	)
	// Relógio do banco: a idade não depende de o relógio desta máquina estar certo.
	err := r.tx.DB(ctx).QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM outbox_events WHERE status = 'pending'),
		  (SELECT count(*) FROM outbox_events WHERE status = 'dead'),
		  COALESCE((SELECT EXTRACT(EPOCH FROM now() - min(next_attempt_at))
		              FROM outbox_events WHERE status = 'pending'), 0)::float8,
		  (SELECT count(*) FROM payments WHERE status IN ('created', 'unknown'))`).
		Scan(&b.OutboxPending, &b.OutboxDead, &oldestSecs, &b.PaymentsStuck)
	if err != nil {
		return observability.Backlog{}, fmt.Errorf("ler backlog: %w", err)
	}
	if oldestSecs > 0 {
		b.OutboxOldestPending = time.Duration(oldestSecs * float64(time.Second))
	}
	return b, nil
}
