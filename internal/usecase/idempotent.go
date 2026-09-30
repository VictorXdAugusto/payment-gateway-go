package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/idempotency"
)

// runIdempotent é o esqueleto comum das operações de um passo só (captura, cancelamento,
// estorno): adquire a chave, devolve a resposta guardada num replay, senão executa run e,
// se falhar, solta o lock para o retry do cliente retomar logo. run é quem chama Finish
// (dentro da própria transação, junto dos efeitos). A criação de pagamento NÃO usa isto:
// ela tem recovery points em várias fases.
func runIdempotent(ctx context.Context, keys idempotency.Store, merchantID, key, hash string,
	run func(acq idempotency.Acquisition) (PaymentView, error)) (CreatePaymentOutput, error) {
	acq, err := keys.Acquire(ctx, idempotency.Key{MerchantID: merchantID, Value: key}, hash)
	if err != nil {
		return CreatePaymentOutput{}, err
	}
	if acq.Replay != nil {
		var view PaymentView
		if err := json.Unmarshal(acq.Replay, &view); err != nil {
			return CreatePaymentOutput{}, fmt.Errorf("resposta guardada corrompida: %w", err)
		}
		return CreatePaymentOutput{Payment: view, Replayed: true}, nil
	}

	view, err := run(acq)
	if err != nil {
		// Contexto sem cancelamento: se o cliente desistiu, ainda precisamos soltar.
		// (Se o lock foi perdido, não é mais nosso para soltar.)
		if !errors.Is(err, idempotency.ErrLockLost) {
			_ = keys.Release(context.WithoutCancel(ctx), acq)
		}
		return CreatePaymentOutput{}, err
	}
	return CreatePaymentOutput{Payment: view}, nil
}
