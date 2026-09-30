package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/outbox"
)

type OutboxRepository struct {
	tx *TxManager
}

var _ outbox.Repository = (*OutboxRepository)(nil)

func NewOutboxRepository(tx *TxManager) *OutboxRepository { return &OutboxRepository{tx: tx} }

// Add participa da transação do contexto: o evento entra (ou desfaz) junto com o estado.
func (r *OutboxRepository) Add(ctx context.Context, events ...outbox.Event) error {
	db := r.tx.DB(ctx)
	for _, e := range events {
		if _, err := db.Exec(ctx, `
			INSERT INTO outbox_events (event_id, merchant_id, type, payment_id, payload, created_at)
			VALUES ($1, $2::uuid, $3, $4, $5, $6)`,
			e.EventID, e.MerchantID, e.Type, e.PaymentID, e.Payload, e.CreatedAt); err != nil {
			return fmt.Errorf("gravar evento %s: %w", e.Type, err)
		}
	}
	return nil
}

// Claim usa FOR UPDATE SKIP LOCKED: dois workers simultâneos nunca pegam a mesma linha e
// nenhum espera pelo outro. O lease (locked_until) cobre o caso de o worker morrer: passado
// o prazo, a linha volta a ficar disponível para qualquer outro.
func (r *OutboxRepository) Claim(ctx context.Context, limit int, lease time.Duration) ([]outbox.Delivery, error) {
	rows, err := r.tx.DB(ctx).Query(ctx, `
		WITH picked AS (
			SELECT id FROM outbox_events
			 WHERE status = 'pending'
			   AND next_attempt_at <= now()
			   AND (locked_until IS NULL OR locked_until < now())
			 ORDER BY next_attempt_at, id
			 LIMIT $1
			 FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox_events e
		   SET locked_until = now() + make_interval(secs => $2::float8),
		       attempts     = e.attempts + 1
		  FROM picked, merchants m
		 WHERE e.id = picked.id AND m.id = e.merchant_id
		RETURNING e.id, e.event_id, e.merchant_id::text, e.type, e.payment_id, e.payload,
		          e.created_at, e.attempts, COALESCE(m.webhook_url, ''), COALESCE(m.webhook_secret, '')`,
		limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("reivindicar entregas: %w", err)
	}
	defer rows.Close()

	var out []outbox.Delivery
	for rows.Next() {
		var d outbox.Delivery
		if err := rows.Scan(&d.ID, &d.Event.EventID, &d.Event.MerchantID, &d.Event.Type, &d.Event.PaymentID,
			&d.Event.Payload, &d.Event.CreatedAt, &d.Attempt, &d.WebhookURL, &d.Secret); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Todos os resultados são "fenceados" por (id, status pendente, attempts): se o lease venceu e
// outro worker reivindicou de novo, attempts avançou e o resultado do worker antigo é descartado.
const fence = `WHERE id = $1 AND status = 'pending' AND attempts = $2`

func (r *OutboxRepository) MarkDelivered(ctx context.Context, d outbox.Delivery) error {
	return r.finish(ctx, `
		UPDATE outbox_events
		   SET status = 'delivered', delivered_at = now(), locked_until = NULL, last_error = ''
		 `+fence, d)
}

func (r *OutboxRepository) Reschedule(ctx context.Context, d outbox.Delivery, delay time.Duration, lastError string) error {
	return r.finish(ctx, `
		UPDATE outbox_events
		   SET next_attempt_at = now() + make_interval(secs => $3::float8), locked_until = NULL, last_error = $4
		 `+fence, d, delay.Seconds(), lastError)
}

func (r *OutboxRepository) MarkDead(ctx context.Context, d outbox.Delivery, lastError string) error {
	return r.finish(ctx, `
		UPDATE outbox_events SET status = 'dead', locked_until = NULL, last_error = $3
		 `+fence, d, lastError)
}

func (r *OutboxRepository) MarkSkipped(ctx context.Context, d outbox.Delivery, reason string) error {
	return r.finish(ctx, `
		UPDATE outbox_events SET status = 'skipped', locked_until = NULL, last_error = $3
		 `+fence, d, reason)
}

func (r *OutboxRepository) finish(ctx context.Context, sql string, d outbox.Delivery, extra ...any) error {
	args := append([]any{d.ID, d.Attempt}, extra...)
	tag, err := r.tx.DB(ctx).Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("atualizar entrega %d: %w", d.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return outbox.ErrClaimLost
	}
	return nil
}

// Purge apaga eventos já entregues ou dispensados há mais que olderThan (relógio do banco).
// Eventos dead e pendentes nunca são apagados: um está esperando inspeção, o outro entrega.
// Apaga no máximo limit por chamada, para não segurar um lock longo na tabela.
func (r *OutboxRepository) Purge(ctx context.Context, olderThan time.Duration, limit int) (int64, error) {
	tag, err := r.tx.DB(ctx).Exec(ctx, `
		DELETE FROM outbox_events
		 WHERE id IN (
		       SELECT id FROM outbox_events
		        WHERE status IN ('delivered', 'skipped')
		          AND created_at < now() - make_interval(secs => $1::float8)
		        ORDER BY created_at
		        LIMIT $2)`, olderThan.Seconds(), limit)
	if err != nil {
		return 0, fmt.Errorf("limpar outbox: %w", err)
	}
	return tag.RowsAffected(), nil
}
