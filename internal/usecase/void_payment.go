package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/idempotency"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/psp"
)

// ErrVoidRejected: o PSP recusou o cancelamento de forma definitiva (ex.: já capturado).
var ErrVoidRejected = errors.New("cancelamento recusado pelo PSP")

type VoidPaymentInput struct {
	MerchantID     string
	PaymentID      string
	IdempotencyKey string
}

type VoidPayment struct {
	tx       TxRunner
	payments payment.Repository
	keys     idempotency.Store
	gateway  psp.Gateway
	events   *EventRecorder
	now      func() time.Time
}

func NewVoidPayment(tx TxRunner, payments payment.Repository, keys idempotency.Store, gateway psp.Gateway,
	events *EventRecorder, now func() time.Time) *VoidPayment {
	return &VoidPayment{tx: tx, payments: payments, keys: keys, gateway: gateway, events: events, now: now}
}

// Execute cancela uma autorização que ainda não foi capturada. Nenhum dinheiro se moveu,
// então não há lançamento no ledger: só o estado e o evento payment.voided.
//
// Mesma ordem deliberada da captura: PSP primeiro (fora de transação, idempotente pela chave
// "void:<id>"), banco depois. Um retry depois de queda repete o cancelamento no PSP (mesmo
// resultado) e só então grava.
func (uc *VoidPayment) Execute(ctx context.Context, in VoidPaymentInput) (CreatePaymentOutput, error) {
	if err := idempotency.ValidateKey(in.IdempotencyKey); err != nil {
		return CreatePaymentOutput{}, err
	}
	hash := idempotency.Fingerprint("void_payment", in.PaymentID)
	return runIdempotent(ctx, uc.keys, in.MerchantID, in.IdempotencyKey, hash,
		func(acq idempotency.Acquisition) (PaymentView, error) { return uc.run(ctx, acq, in) })
}

func (uc *VoidPayment) run(ctx context.Context, acq idempotency.Acquisition, in VoidPaymentInput) (PaymentView, error) {
	p, err := uc.payments.Get(ctx, payment.MerchantID(in.MerchantID), payment.ID(in.PaymentID))
	if err != nil {
		return PaymentView{}, err
	}
	// Valida a transição ANTES de incomodar o PSP (e deixa o agregado pronto para gravar).
	if err := p.Void(uc.now()); err != nil {
		return PaymentView{}, err
	}

	err = uc.gateway.Void(ctx, psp.VoidRequest{IdempotencyKey: "void:" + string(p.ID()), Reference: p.PSPReference()})
	var declined *psp.DeclinedError
	switch {
	case err == nil:
	case errors.As(err, &declined):
		return PaymentView{}, fmt.Errorf("%w: %s", ErrVoidRejected, declined.Code)
	case errors.Is(err, psp.ErrIndeterminate):
		return PaymentView{}, fmt.Errorf("%w: %v", ErrPSPUnavailable, err)
	default:
		return PaymentView{}, err
	}

	body, err := json.Marshal(viewOf(p))
	if err != nil {
		return PaymentView{}, err
	}
	err = uc.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := uc.payments.Update(ctx, p); err != nil { // lock otimista contra captura concorrente
			return err
		}
		if err := uc.events.Record(ctx, p); err != nil { // payment.voided
			return err
		}
		return uc.keys.Finish(ctx, acq, body)
	})
	if err != nil {
		return PaymentView{}, err
	}
	return viewOf(p), nil
}
