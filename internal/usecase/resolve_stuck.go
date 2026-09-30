package usecase

import (
	"context"
	"errors"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/psp"
)

// ReasonPSPNeverReceived: o PSP não conhece a tentativa, depois do período de carência.
const ReasonPSPNeverReceived = "psp_never_received"

// ResolveStuck descobre o desfecho de um pagamento preso perguntando ao PSP. Dois casos:
//   - unknown: o PSP não respondeu e não sabemos se autorizou;
//   - created parado: o processo caiu depois de gravar o pagamento e antes de ter um desfecho,
//     e o cliente nunca repetiu a requisição.
//
// É a peça central da reconciliação: o Reconciler (internal/worker) a chama periodicamente.
type ResolveStuck struct {
	tx       TxRunner
	payments payment.Repository
	gateway  psp.Gateway
	events   *EventRecorder
	grace    time.Duration
	now      func() time.Time
}

// grace: quanto esperar antes de concluir "o PSP nunca recebeu". Um PSP pode estar com a
// requisição numa fila; concluir cedo demais transformaria uma aprovação tardia em dinheiro
// retido de um pagamento que já demos como falho.
func NewResolveStuck(tx TxRunner, payments payment.Repository, gateway psp.Gateway, events *EventRecorder,
	grace time.Duration, now func() time.Time) *ResolveStuck {
	return &ResolveStuck{tx: tx, payments: payments, gateway: gateway, events: events, grace: grace, now: now}
}

// Execute é idempotente e seguro de rodar em paralelo: só age se o pagamento AINDA estiver
// preso (created ou unknown), e a gravação usa lock otimista (dois reconciliadores, ou um
// reconciliador e uma requisição viva, não se sobrescrevem). Devolve o status depois da
// tentativa (pode continuar preso).
func (uc *ResolveStuck) Execute(ctx context.Context, id payment.ID) (payment.Status, error) {
	p, err := uc.payments.GetByID(ctx, id)
	if err != nil {
		return "", err
	}
	if p.Status() != payment.StatusUnknown && p.Status() != payment.StatusCreated {
		return p.Status(), nil // já resolvido por outro caminho
	}

	stuck := p.Status()
	res, err := uc.gateway.Lookup(ctx, string(p.ID()))
	if err != nil {
		return stuck, err // PSP fora do ar agora: tenta na próxima rodada
	}

	now := uc.now()
	switch res.Outcome {
	case psp.OutcomeAuthorized:
		err = p.Authorize(res.Reference, now)
	case psp.OutcomeDeclined:
		err = p.Fail(res.DeclineCode, now)
	case psp.OutcomeNotFound:
		if now.Sub(p.UpdatedAt()) < uc.grace {
			return stuck, nil // cedo demais para concluir
		}
		err = p.Fail(ReasonPSPNeverReceived, now)
	default:
		return stuck, errors.New("desfecho de lookup desconhecido: " + string(res.Outcome))
	}
	if err != nil {
		return "", err
	}

	// O desfecho e o evento para o lojista (payment.authorized / payment.failed) andam juntos.
	err = uc.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := uc.payments.Update(ctx, p); err != nil {
			return err
		}
		return uc.events.Record(ctx, p)
	})
	if err != nil {
		if errors.Is(err, payment.ErrConcurrentModification) {
			// Outro reconciliador chegou primeiro: releia e devolva o que ficou valendo.
			cur, gerr := uc.payments.GetByID(ctx, id)
			if gerr != nil {
				return "", gerr
			}
			return cur.Status(), nil
		}
		return "", err
	}
	return p.Status(), nil
}
