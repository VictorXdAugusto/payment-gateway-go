package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/idempotency"
)

// pointPaymentCreated: o pagamento já foi gravado. Um retry a partir daqui NÃO cria outro.
const pointPaymentCreated = "payment_created"

// TxRunner executa fn numa transação; *postgres.TxManager satisfaz esta interface.
type TxRunner interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

type CreatePaymentInput struct {
	MerchantID     string
	IdempotencyKey string
	Amount         int64
	Currency       string
}

type CreatePaymentOutput struct {
	Payment  PaymentView
	Replayed bool // true quando é a resposta guardada de uma execução anterior
}

type CreatePayment struct {
	tx       TxRunner
	payments payment.Repository
	keys     idempotency.Store
	newID    func() payment.ID
	now      func() time.Time
}

func NewCreatePayment(tx TxRunner, payments payment.Repository, keys idempotency.Store,
	newID func() payment.ID, now func() time.Time) *CreatePayment {
	return &CreatePayment{tx: tx, payments: payments, keys: keys, newID: newID, now: now}
}

// Execute cria um pagamento no máximo UMA vez por (lojista, chave), aconteça o que acontecer:
// retry do cliente, requisições concorrentes ou queda do processo no meio.
func (uc *CreatePayment) Execute(ctx context.Context, in CreatePaymentInput) (CreatePaymentOutput, error) {
	// 1) Valida TUDO antes de tocar na chave: requisição inválida não consome a chave.
	if err := idempotency.ValidateKey(in.IdempotencyKey); err != nil {
		return CreatePaymentOutput{}, err
	}
	amount, err := parseAmount(in.Amount, in.Currency)
	if err != nil {
		return CreatePaymentOutput{}, err
	}

	// 2) Adquire a chave. O hash amarra a chave ao corpo: mesma chave + corpo diferente = erro.
	hash := idempotency.Fingerprint("create_payment", strconv.FormatInt(in.Amount, 10), string(amount.Currency()))
	acq, err := uc.keys.Acquire(ctx, idempotency.Key{MerchantID: in.MerchantID, Value: in.IdempotencyKey}, hash)
	if err != nil {
		return CreatePaymentOutput{}, err
	}

	// 3) Já terminou antes: devolve a resposta original, sem reexecutar nada.
	if acq.Replay != nil {
		var view PaymentView
		if err := json.Unmarshal(acq.Replay, &view); err != nil {
			return CreatePaymentOutput{}, fmt.Errorf("resposta guardada corrompida: %w", err)
		}
		return CreatePaymentOutput{Payment: view, Replayed: true}, nil
	}

	view, err := uc.run(ctx, acq, in, amount)
	if err != nil {
		// Solta o lock para o retry do cliente retomar já, sem esperar o lease expirar.
		// Contexto sem cancelamento: se o cliente desistiu, ainda precisamos soltar.
		// (Se o lock foi perdido, não é mais nosso para soltar.)
		if !errors.Is(err, idempotency.ErrLockLost) {
			_ = uc.keys.Release(context.WithoutCancel(ctx), acq)
		}
		return CreatePaymentOutput{}, err
	}
	return CreatePaymentOutput{Payment: view}, nil
}

// run executa as fases a partir do recovery point. Cada fase grava seus efeitos e avança
// o ponto NA MESMA TRANSAÇÃO: não existe estado "criou mas não anotou".
func (uc *CreatePayment) run(ctx context.Context, acq idempotency.Acquisition, in CreatePaymentInput, amount money.Money) (PaymentView, error) {
	paymentID := payment.ID(acq.ResourceID)

	switch acq.RecoveryPoint {
	case idempotency.PointStarted:
		// Fase 1: criar o pagamento.
		p, err := payment.New(uc.newID(), payment.MerchantID(in.MerchantID), amount, uc.now())
		if err != nil {
			return PaymentView{}, err
		}
		// Os eventos de domínio (payment.created) viram linhas da outbox no passo 6,
		// nesta mesma transação. Por ora ficam no agregado.
		err = uc.tx.WithinTx(ctx, func(ctx context.Context) error {
			if err := uc.payments.Insert(ctx, p, in.IdempotencyKey); err != nil {
				return err
			}
			return uc.keys.Advance(ctx, acq, pointPaymentCreated, string(p.ID()))
		})
		if err != nil {
			return PaymentView{}, err
		}
		paymentID = p.ID()

	case pointPaymentCreated:
		// A fase 1 já foi commitada por uma execução anterior: retoma daqui.

	default:
		return PaymentView{}, fmt.Errorf("recovery point desconhecido %q", acq.RecoveryPoint)
	}

	// Fase final: relê do banco (assim a 1ª resposta, o replay e o GET são idênticos) e finaliza.
	p, err := uc.payments.Get(ctx, payment.MerchantID(in.MerchantID), paymentID)
	if err != nil {
		return PaymentView{}, err
	}
	view := viewOf(p)
	body, err := json.Marshal(view)
	if err != nil {
		return PaymentView{}, err
	}
	if err := uc.tx.WithinTx(ctx, func(ctx context.Context) error {
		return uc.keys.Finish(ctx, acq, body)
	}); err != nil {
		return PaymentView{}, err
	}
	return view, nil
}

func parseAmount(cents int64, currency string) (money.Money, error) {
	cur, err := money.ParseCurrency(currency)
	if err != nil {
		return money.Money{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if cents <= 0 {
		return money.Money{}, fmt.Errorf("%w: amount deve ser maior que zero", ErrInvalidInput)
	}
	return money.New(cents, cur)
}
