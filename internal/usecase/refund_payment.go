package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/ledger"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/idempotency"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/psp"
)

// ErrRefundRejected: o PSP recusou o estorno de forma definitiva.
var ErrRefundRejected = errors.New("estorno recusado pelo PSP")

type RefundPaymentInput struct {
	MerchantID     string
	PaymentID      string
	Amount         int64 // centavos, na moeda do pagamento
	IdempotencyKey string
}

type RefundPayment struct {
	tx       TxRunner
	payments payment.Repository
	ledger   ledger.Repository
	keys     idempotency.Store
	gateway  psp.Gateway
	events   *EventRecorder
	newTxID  func() ledger.TransactionID
	now      func() time.Time
}

func NewRefundPayment(tx TxRunner, payments payment.Repository, lg ledger.Repository, keys idempotency.Store,
	gateway psp.Gateway, events *EventRecorder, newTxID func() ledger.TransactionID, now func() time.Time) *RefundPayment {
	return &RefundPayment{tx: tx, payments: payments, ledger: lg, keys: keys, gateway: gateway,
		events: events, newTxID: newTxID, now: now}
}

// Execute estorna parte ou todo o valor capturado. Um pagamento aceita vários estornos.
//
// Cada estorno é identificado pela Idempotency-Key do cliente: a chave do PSP
// ("refund:<pagamento>:<chave>") é estável entre retries e distinta entre estornos, então
// repetir NUNCA devolve o dinheiro duas vezes e dois estornos parciais diferentes não se confundem.
//
// Uma transação grava: pagamento + lançamento no ledger + evento + chave finalizada.
func (uc *RefundPayment) Execute(ctx context.Context, in RefundPaymentInput) (CreatePaymentOutput, error) {
	if err := idempotency.ValidateKey(in.IdempotencyKey); err != nil {
		return CreatePaymentOutput{}, err
	}
	if in.Amount <= 0 {
		return CreatePaymentOutput{}, fmt.Errorf("%w: amount deve ser maior que zero", ErrInvalidInput)
	}
	hash := idempotency.Fingerprint("refund_payment", in.PaymentID, strconv.FormatInt(in.Amount, 10))
	return runIdempotent(ctx, uc.keys, in.MerchantID, in.IdempotencyKey, hash,
		func(acq idempotency.Acquisition) (PaymentView, error) { return uc.run(ctx, acq, in) })
}

func (uc *RefundPayment) run(ctx context.Context, acq idempotency.Acquisition, in RefundPaymentInput) (PaymentView, error) {
	p, err := uc.payments.Get(ctx, payment.MerchantID(in.MerchantID), payment.ID(in.PaymentID))
	if err != nil {
		return PaymentView{}, err
	}
	amount, err := money.New(in.Amount, p.Amount().Currency())
	if err != nil {
		return PaymentView{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}

	// Valida estado e limite ANTES de incomodar o PSP (e deixa o agregado pronto para gravar).
	now := uc.now()
	if err := p.Refund(amount, now); err != nil {
		return PaymentView{}, err
	}

	refundID := string(p.ID()) + ":" + in.IdempotencyKey
	err = uc.gateway.Refund(ctx, psp.RefundRequest{
		IdempotencyKey: "refund:" + refundID, Reference: p.PSPReference(), Amount: amount,
	})
	var declined *psp.DeclinedError
	switch {
	case err == nil:
	case errors.As(err, &declined):
		return PaymentView{}, fmt.Errorf("%w: %s", ErrRefundRejected, declined.Code)
	case errors.Is(err, psp.ErrIndeterminate):
		return PaymentView{}, fmt.Errorf("%w: %v", ErrPSPUnavailable, err)
	default:
		return PaymentView{}, err
	}

	entry, err := ledger.Refund(uc.newTxID(), refundID, string(p.ID()), in.MerchantID, amount, now)
	if err != nil {
		return PaymentView{}, err
	}
	body, err := json.Marshal(viewOf(p))
	if err != nil {
		return PaymentView{}, err
	}

	err = uc.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := uc.payments.Update(ctx, p); err != nil { // lock otimista: estornos concorrentes se serializam aqui
			return err
		}
		if err := uc.ledger.Post(ctx, entry); err != nil {
			return err
		}
		if err := uc.events.Record(ctx, p); err != nil { // payment.refunded
			return err
		}
		return uc.keys.Finish(ctx, acq, body)
	})
	if err != nil {
		return PaymentView{}, err
	}
	return viewOf(p), nil
}
