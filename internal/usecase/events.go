package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/outbox"
)

// EventRecorder converte os eventos de domínio acumulados num agregado em linhas da outbox.
// Deve ser chamado DENTRO da transação que grava o estado do agregado: é isso que faz o
// evento existir se, e somente se, a mudança de estado existir.
type EventRecorder struct {
	repo  outbox.Repository
	newID func() string
}

func NewEventRecorder(repo outbox.Repository, newID func() string) *EventRecorder {
	return &EventRecorder{repo: repo, newID: newID}
}

// envelope é o corpo que o lojista recebe.
type envelope struct {
	ID        string    `json:"id"` // igual ao X-Gateway-Event-Id; use para deduplicar
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Data      eventData `json:"data"`
}

type eventData struct {
	Payment  PaymentView `json:"payment"`
	Amount   int64       `json:"amount"` // valor DO FATO (num estorno, o valor daquele estorno)
	Currency string      `json:"currency"`
	Reason   string      `json:"reason,omitempty"`
}

// Record esvazia a fila de eventos do agregado e os grava na outbox.
func (r *EventRecorder) Record(ctx context.Context, p *payment.Payment) error {
	domainEvents := p.PullEvents()
	if len(domainEvents) == 0 {
		return nil
	}

	view := viewOf(p)
	out := make([]outbox.Event, 0, len(domainEvents))
	for _, e := range domainEvents {
		id := r.newID()
		body, err := json.Marshal(envelope{
			ID: id, Type: string(e.Type), CreatedAt: e.OccurredAt.UTC(),
			Data: eventData{Payment: view, Amount: e.Amount.Amount(), Currency: e.Amount.Currency().String(), Reason: e.Reason},
		})
		if err != nil {
			return fmt.Errorf("serializar evento %s: %w", e.Type, err)
		}
		out = append(out, outbox.Event{
			EventID: id, MerchantID: string(e.MerchantID), Type: string(e.Type),
			PaymentID: string(e.PaymentID), Payload: body, CreatedAt: e.OccurredAt.UTC(),
		})
	}
	return r.repo.Add(ctx, out...)
}
