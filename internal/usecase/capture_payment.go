package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/ledger"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/idempotency"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/psp"
)

var (
	// ErrPSPUnavailable: o PSP não confirmou a captura. Nada mudou aqui; repetir a
	// requisição com a MESMA Idempotency-Key é seguro (o PSP também é idempotente).
	ErrPSPUnavailable = errors.New("PSP indisponível")

	// ErrCaptureRejected: o PSP recusou a captura de forma definitiva.
	ErrCaptureRejected = errors.New("captura recusada pelo PSP")
)

type CapturePaymentInput struct {
	MerchantID     string
	PaymentID      string
	IdempotencyKey string
}

type CapturePayment struct {
	tx       TxRunner
	payments payment.Repository
	ledger   ledger.Repository
	keys     idempotency.Store
	gateway  psp.Gateway
	feeBps   int64
	newTxID  func() ledger.TransactionID
	now      func() time.Time
}

func NewCapturePayment(tx TxRunner, payments payment.Repository, lg ledger.Repository, keys idempotency.Store,
	gateway psp.Gateway, feeBasisPoints int64, newTxID func() ledger.TransactionID, now func() time.Time) *CapturePayment {
	return &CapturePayment{tx: tx, payments: payments, ledger: lg, keys: keys, gateway: gateway,
		feeBps: feeBasisPoints, newTxID: newTxID, now: now}
}

// Execute captura um pagamento autorizado.
//
// Ordem deliberada: PSP primeiro (fora de transação), banco depois. Se o processo cair no
// meio, o retry repete a captura no PSP (idempotente pela chave "capture:<id>") e só então
// grava. O que NÃO pode acontecer, e não acontece, é gravar "capturado" sem o PSP ter capturado.
//
// Uma única transação grava: pagamento capturado + lançamentos do ledger + chave finalizada.
// Ou os três entram, ou nenhum.
func (uc *CapturePayment) Execute(ctx context.Context, in CapturePaymentInput) (CreatePaymentOutput, error) {
	if err := idempotency.ValidateKey(in.IdempotencyKey); err != nil {
		return CreatePaymentOutput{}, err
	}

	hash := idempotency.Fingerprint("capture_payment", in.PaymentID)
	acq, err := uc.keys.Acquire(ctx, idempotency.Key{MerchantID: in.MerchantID, Value: in.IdempotencyKey}, hash)
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

	view, err := uc.run(ctx, acq, in)
	if err != nil {
		if !errors.Is(err, idempotency.ErrLockLost) {
			_ = uc.keys.Release(context.WithoutCancel(ctx), acq)
		}
		return CreatePaymentOutput{}, err
	}
	return CreatePaymentOutput{Payment: view}, nil
}

func (uc *CapturePayment) run(ctx context.Context, acq idempotency.Acquisition, in CapturePaymentInput) (PaymentView, error) {
	merchantID := payment.MerchantID(in.MerchantID)

	p, err := uc.payments.Get(ctx, merchantID, payment.ID(in.PaymentID))
	if err != nil {
		return PaymentView{}, err
	}
	// Recusa cedo, antes de incomodar o PSP: só se captura o que está autorizado.
	if !p.Status().CanTransitionTo(payment.StatusCaptured) {
		return PaymentView{}, &payment.TransitionError{Operation: "capture", From: p.Status(), To: payment.StatusCaptured}
	}

	err = uc.gateway.Capture(ctx, psp.CaptureRequest{
		IdempotencyKey: "capture:" + string(p.ID()),
		Reference:      p.PSPReference(),
		Amount:         p.Amount(),
	})
	var declined *psp.DeclinedError
	switch {
	case err == nil:
	case errors.As(err, &declined):
		return PaymentView{}, fmt.Errorf("%w: %s", ErrCaptureRejected, declined.Code)
	case errors.Is(err, psp.ErrIndeterminate):
		return PaymentView{}, fmt.Errorf("%w: %v", ErrPSPUnavailable, err)
	default:
		return PaymentView{}, err
	}

	now := uc.now()
	if err := p.Capture(now); err != nil {
		return PaymentView{}, err
	}
	entry, err := ledger.Capture(uc.newTxID(), string(p.ID()), in.MerchantID, p.Amount(), uc.feeBps, now)
	if err != nil {
		return PaymentView{}, err
	}
	body, err := json.Marshal(viewOf(p))
	if err != nil {
		return PaymentView{}, err
	}

	err = uc.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := uc.payments.Update(ctx, p); err != nil { // lock otimista contra void/capture concorrentes
			return err
		}
		if err := uc.ledger.Post(ctx, entry); err != nil {
			return err
		}
		return uc.keys.Finish(ctx, acq, body)
	})
	if err != nil {
		return PaymentView{}, err
	}
	return viewOf(p), nil
}
