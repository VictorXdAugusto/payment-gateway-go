package payment

import (
	"fmt"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
)

type (
	ID         string
	MerchantID string
)

// Payment é o agregado raiz. Os campos são privados de propósito: a única forma de
// mudar o estado é pelos métodos abaixo, que garantem as transições e os invariantes.
//
// Todo método que muda estado recebe `now`: o domínio não chama time.Now(), então
// os testes são determinísticos e o relógio é decisão de quem orquestra.
type Payment struct {
	id            ID
	merchantID    MerchantID
	amount        money.Money
	refunded      money.Money
	status        Status
	pspReference  string
	failureReason string
	version       int64
	createdAt     time.Time
	updatedAt     time.Time

	events []Event
}

// New cria um pagamento no estado Created.
func New(id ID, merchantID MerchantID, amount money.Money, now time.Time) (*Payment, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: id", ErrMissingField)
	}
	if merchantID == "" {
		return nil, fmt.Errorf("%w: merchant_id", ErrMissingField)
	}
	if !amount.HasCurrency() {
		return nil, fmt.Errorf("%w: moeda", ErrMissingField)
	}
	if !amount.IsPositive() {
		return nil, fmt.Errorf("%w: %s (deve ser maior que zero)", ErrInvalidAmount, amount)
	}

	zero, err := money.New(0, amount.Currency())
	if err != nil {
		return nil, err
	}

	p := &Payment{
		id:         id,
		merchantID: merchantID,
		amount:     amount,
		refunded:   zero,
		status:     StatusCreated,
		createdAt:  now,
		updatedAt:  now,
	}
	p.record(EventCreated, amount, "", now)
	return p, nil
}

// Authorize registra que o PSP autorizou. Vale a partir de Created ou de Unknown
// (quando a reconciliação descobre que a autorização tinha acontecido).
func (p *Payment) Authorize(pspReference string, now time.Time) error {
	if err := p.checkTransition("authorize", StatusAuthorized); err != nil {
		return err
	}
	if pspReference == "" {
		return fmt.Errorf("%w: psp_reference", ErrMissingField)
	}
	p.status = StatusAuthorized
	p.pspReference = pspReference
	p.touch(now)
	p.record(EventAuthorized, p.amount, "", now)
	return nil
}

// Fail registra que a autorização foi negada (ou que a reconciliação confirmou que não ocorreu).
func (p *Payment) Fail(reason string, now time.Time) error {
	if err := p.checkTransition("fail", StatusFailed); err != nil {
		return err
	}
	p.status = StatusFailed
	p.failureReason = reason
	p.touch(now)
	p.record(EventFailed, p.amount, reason, now)
	return nil
}

// MarkUnknown registra que o PSP não respondeu. Não gera evento para o lojista:
// para ele o pagamento ainda está em andamento.
func (p *Payment) MarkUnknown(now time.Time) error {
	if err := p.checkTransition("mark_unknown", StatusUnknown); err != nil {
		return err
	}
	p.status = StatusUnknown
	p.touch(now)
	return nil
}

// Capture efetiva a cobrança do valor autorizado (captura total).
func (p *Payment) Capture(now time.Time) error {
	if err := p.checkTransition("capture", StatusCaptured); err != nil {
		return err
	}
	p.status = StatusCaptured
	p.touch(now)
	p.record(EventCaptured, p.amount, "", now)
	return nil
}

// Void cancela uma autorização que ainda não foi capturada. Nada foi cobrado.
func (p *Payment) Void(now time.Time) error {
	if err := p.checkTransition("void", StatusVoided); err != nil {
		return err
	}
	p.status = StatusVoided
	p.touch(now)
	p.record(EventVoided, p.amount, "", now)
	return nil
}

// Refund devolve parte ou todo o valor capturado. Pode ser chamado várias vezes até
// o total estornado igualar o capturado.
func (p *Payment) Refund(amount money.Money, now time.Time) error {
	// Estado antes de valor: erro de transição tem precedência sobre erro de argumento.
	if err := p.checkTransition("refund", StatusRefunded); err != nil {
		return err
	}
	if !amount.IsPositive() {
		return fmt.Errorf("%w: estorno de %s (deve ser maior que zero)", ErrInvalidAmount, amount)
	}

	total, err := p.refunded.Add(amount) // falha se a moeda for outra
	if err != nil {
		return err
	}
	cmp, err := total.Compare(p.amount)
	if err != nil {
		return err
	}
	if cmp > 0 {
		return fmt.Errorf("%w: pedido %s, restante %s", ErrRefundExceedsCaptured, amount, p.RefundableAmount())
	}

	p.refunded = total
	if cmp == 0 {
		p.status = StatusRefunded
	} else {
		p.status = StatusPartiallyRefunded
	}
	p.touch(now)
	p.record(EventRefunded, amount, "", now)
	return nil
}

// checkTransition só valida contra a tabela, sem mudar nada. Cada método valida a
// transição e os argumentos ANTES de mutar, para que uma operação que falha nunca
// deixe o agregado pela metade.
func (p *Payment) checkTransition(operation string, to Status) error {
	if !p.status.CanTransitionTo(to) {
		return &TransitionError{Operation: operation, From: p.status, To: to}
	}
	return nil
}

func (p *Payment) touch(now time.Time) { p.updatedAt = now }

func (p *Payment) record(t EventType, amount money.Money, reason string, now time.Time) {
	p.events = append(p.events, Event{
		Type: t, PaymentID: p.id, MerchantID: p.merchantID,
		Amount: amount, Reason: reason, OccurredAt: now,
	})
}

// PullEvents devolve os eventos acumulados e zera a fila. Quem chama é responsável
// por persisti-los junto com o estado.
func (p *Payment) PullEvents() []Event {
	out := p.events
	p.events = nil
	return out
}

// RefundableAmount é quanto ainda pode ser estornado (zero fora dos estados estornáveis).
func (p *Payment) RefundableAmount() money.Money {
	if !p.status.CanTransitionTo(StatusRefunded) {
		zero, _ := money.New(0, p.amount.Currency())
		return zero
	}
	rest, _ := p.amount.Sub(p.refunded) // mesma moeda por construção; refunded <= amount
	return rest
}

func (p *Payment) ID() ID                      { return p.id }
func (p *Payment) MerchantID() MerchantID      { return p.merchantID }
func (p *Payment) Amount() money.Money         { return p.amount }
func (p *Payment) RefundedAmount() money.Money { return p.refunded }
func (p *Payment) Status() Status              { return p.status }
func (p *Payment) PSPReference() string        { return p.pspReference }
func (p *Payment) FailureReason() string       { return p.failureReason }
func (p *Payment) Version() int64              { return p.version }
func (p *Payment) CreatedAt() time.Time        { return p.createdAt }
func (p *Payment) UpdatedAt() time.Time        { return p.updatedAt }
