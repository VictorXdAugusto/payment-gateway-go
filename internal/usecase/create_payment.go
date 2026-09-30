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
	"github.com/VictorXdAugusto/payment-gateway-go/internal/psp"
)

// Recovery points de CreatePayment, em ordem:
//
//	started -> payment_created -> psp_resolved -> finished
//
//	payment_created: o pagamento existe. Um retry daqui NÃO cria outro.
//	psp_resolved:    o desfecho do PSP (aprovado, recusado ou desconhecido) já foi gravado.
//	                 Um retry daqui NÃO chama o PSP de novo.
const (
	pointPaymentCreated = "payment_created"
	pointPSPResolved    = "psp_resolved"
)

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
	gateway  psp.Gateway
	newID    func() payment.ID
	now      func() time.Time
}

func NewCreatePayment(tx TxRunner, payments payment.Repository, keys idempotency.Store, gateway psp.Gateway,
	newID func() payment.ID, now func() time.Time) *CreatePayment {
	return &CreatePayment{tx: tx, payments: payments, keys: keys, gateway: gateway, newID: newID, now: now}
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
	merchantID := payment.MerchantID(in.MerchantID)
	paymentID := payment.ID(acq.ResourceID)
	point := acq.RecoveryPoint

	// Fase 1: criar o pagamento.
	if point == idempotency.PointStarted {
		p, err := payment.New(uc.newID(), merchantID, amount, uc.now())
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
		paymentID, point = p.ID(), pointPaymentCreated
	}

	// Fase 2: autorizar no PSP e gravar o desfecho.
	if point == pointPaymentCreated {
		if err := uc.authorize(ctx, acq, merchantID, paymentID); err != nil {
			return PaymentView{}, err
		}
		point = pointPSPResolved
	}

	if point != pointPSPResolved {
		return PaymentView{}, fmt.Errorf("recovery point desconhecido %q", acq.RecoveryPoint)
	}

	// Fase final: relê do banco (assim a 1ª resposta, o replay e o GET são idênticos) e finaliza.
	p, err := uc.payments.Get(ctx, merchantID, paymentID)
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

// authorize chama o PSP FORA de qualquer transação de banco (não se segura conexão nem
// lock enquanto se espera um serviço externo) e grava o desfecho junto com o avanço do ponto.
//
// A chave de idempotência do PSP é o id do PAGAMENTO: estável entre retries. Se o processo
// cair depois do PSP aprovar e antes de gravarmos, o retry chama o PSP de novo, recebe a
// MESMA autorização e segue. Nunca há duas.
func (uc *CreatePayment) authorize(ctx context.Context, acq idempotency.Acquisition, merchantID payment.MerchantID, id payment.ID) error {
	p, err := uc.payments.Get(ctx, merchantID, id)
	if err != nil {
		return err
	}

	changed := false
	if p.Status() == payment.StatusCreated {
		auth, pspErr := uc.gateway.Authorize(ctx, psp.AuthorizeRequest{IdempotencyKey: string(p.ID()), Amount: p.Amount()})

		now := uc.now()
		var declined *psp.DeclinedError
		switch {
		case pspErr == nil:
			err = p.Authorize(auth.Reference, now)
		case errors.As(pspErr, &declined):
			err = p.Fail(declined.Code, now)
		case errors.Is(pspErr, psp.ErrIndeterminate):
			// Não sabemos se aprovou. NÃO assumimos falha: fica unknown até a reconciliação.
			err = p.MarkUnknown(now)
		default:
			// Cancelamento ou bug de contrato: nada foi decidido, o estado não muda.
			return pspErr
		}
		if err != nil {
			return err
		}
		changed = true
	}

	return uc.tx.WithinTx(ctx, func(ctx context.Context) error {
		if changed { // se o pagamento já tinha saído de created, só falta avançar o ponto
			if err := uc.payments.Update(ctx, p); err != nil {
				return err
			}
		}
		return uc.keys.Advance(ctx, acq, pointPSPResolved, "")
	})
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
