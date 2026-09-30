package payment

import (
	"fmt"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
)

// Snapshot é o formato "achatado" do agregado, usado para persistir e reidratar.
// Mantém o Payment encapsulado sem obrigar a infraestrutura a conhecer seus campos privados.
type Snapshot struct {
	ID            ID
	MerchantID    MerchantID
	Amount        money.Money
	Refunded      money.Money
	Status        Status
	PSPReference  string
	FailureReason string
	Version       int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (p *Payment) Snapshot() Snapshot {
	return Snapshot{
		ID: p.id, MerchantID: p.merchantID, Amount: p.amount, Refunded: p.refunded,
		Status: p.status, PSPReference: p.pspReference, FailureReason: p.failureReason,
		Version: p.version, CreatedAt: p.createdAt, UpdatedAt: p.updatedAt,
	}
}

// Restore reconstrói um Payment vindo do banco, VALIDANDO os invariantes.
// Se o dado gravado estiver corrompido, falha alto em vez de operar sobre lixo.
func Restore(s Snapshot) (*Payment, error) {
	if s.ID == "" || s.MerchantID == "" {
		return nil, fmt.Errorf("%w: id/merchant_id", ErrMissingField)
	}
	if !s.Status.IsValid() {
		return nil, fmt.Errorf("%w: status %q desconhecido", ErrInvalidState, s.Status)
	}
	if !s.Amount.HasCurrency() || !s.Amount.IsPositive() {
		return nil, fmt.Errorf("%w: valor %s", ErrInvalidState, s.Amount)
	}
	if s.Refunded.Currency() != s.Amount.Currency() || s.Refunded.IsNegative() {
		return nil, fmt.Errorf("%w: estornado %s", ErrInvalidState, s.Refunded)
	}

	cmp, err := s.Refunded.Compare(s.Amount)
	if err != nil {
		return nil, err
	}
	switch {
	case cmp > 0:
		return nil, fmt.Errorf("%w: estornado %s maior que o valor %s", ErrInvalidState, s.Refunded, s.Amount)
	case s.Status == StatusRefunded && cmp != 0:
		return nil, fmt.Errorf("%w: refunded exige estorno total", ErrInvalidState)
	case s.Status == StatusPartiallyRefunded && (s.Refunded.IsZero() || cmp == 0):
		return nil, fmt.Errorf("%w: partially_refunded exige estorno parcial", ErrInvalidState)
	case s.Status != StatusRefunded && s.Status != StatusPartiallyRefunded && !s.Refunded.IsZero():
		return nil, fmt.Errorf("%w: %s não pode ter valor estornado", ErrInvalidState, s.Status)
	}

	switch s.Status {
	case StatusAuthorized, StatusCaptured, StatusPartiallyRefunded, StatusRefunded, StatusVoided:
		if s.PSPReference == "" {
			return nil, fmt.Errorf("%w: %s exige psp_reference", ErrInvalidState, s.Status)
		}
	}

	return &Payment{
		id: s.ID, merchantID: s.MerchantID, amount: s.Amount, refunded: s.Refunded,
		status: s.Status, pspReference: s.PSPReference, failureReason: s.FailureReason,
		version: s.Version, createdAt: s.CreatedAt, updatedAt: s.UpdatedAt,
	}, nil
}
