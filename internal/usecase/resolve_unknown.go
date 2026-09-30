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

// ResolveUnknown descobre o desfecho de um pagamento em estado unknown perguntando ao PSP.
// É a peça central da reconciliação (o job que a chama periodicamente vem no passo 7).
type ResolveUnknown struct {
	payments payment.Repository
	gateway  psp.Gateway
	grace    time.Duration
	now      func() time.Time
}

// grace: quanto esperar antes de concluir "o PSP nunca recebeu". Um PSP pode estar com a
// requisição numa fila; concluir cedo demais transformaria uma aprovação tardia em dinheiro
// retido de um pagamento que já demos como falho.
func NewResolveUnknown(payments payment.Repository, gateway psp.Gateway, grace time.Duration, now func() time.Time) *ResolveUnknown {
	return &ResolveUnknown{payments: payments, gateway: gateway, grace: grace, now: now}
}

// Execute é idempotente e seguro de rodar em paralelo: só age se o pagamento AINDA for
// unknown, e a gravação usa lock otimista (dois reconciliadores não se sobrescrevem).
// Devolve o status depois da tentativa (pode continuar unknown).
func (uc *ResolveUnknown) Execute(ctx context.Context, id payment.ID) (payment.Status, error) {
	p, err := uc.payments.GetByID(ctx, id)
	if err != nil {
		return "", err
	}
	if p.Status() != payment.StatusUnknown {
		return p.Status(), nil // já resolvido por outro caminho
	}

	res, err := uc.gateway.Lookup(ctx, string(p.ID()))
	if err != nil {
		return payment.StatusUnknown, err // PSP fora do ar agora: tenta na próxima rodada
	}

	now := uc.now()
	switch res.Outcome {
	case psp.OutcomeAuthorized:
		err = p.Authorize(res.Reference, now)
	case psp.OutcomeDeclined:
		err = p.Fail(res.DeclineCode, now)
	case psp.OutcomeNotFound:
		if now.Sub(p.UpdatedAt()) < uc.grace {
			return payment.StatusUnknown, nil // cedo demais para concluir
		}
		err = p.Fail(ReasonPSPNeverReceived, now)
	default:
		return payment.StatusUnknown, errors.New("desfecho de lookup desconhecido: " + string(res.Outcome))
	}
	if err != nil {
		return "", err
	}

	if err := uc.payments.Update(ctx, p); err != nil {
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
