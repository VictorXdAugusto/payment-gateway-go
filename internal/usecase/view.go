// Package usecase orquestra as operações da aplicação sobre o domínio e as portas de persistência.
package usecase

import (
	"errors"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
)

// ErrInvalidInput: a requisição é inválida ANTES de qualquer efeito (não consome chave).
var ErrInvalidInput = errors.New("requisição inválida")

// PaymentView é o recurso "pagamento" como a API o expõe. É também o que fica guardado
// na chave de idempotência, para o replay devolver exatamente a resposta original.
type PaymentView struct {
	ID             string    `json:"id"`
	Status         string    `json:"status"`
	Amount         int64     `json:"amount"`
	Currency       string    `json:"currency"`
	RefundedAmount int64     `json:"refunded_amount"`
	FailureReason  string    `json:"failure_reason,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// StatusProcessing é como o lojista enxerga um pagamento em estado unknown: o desfecho
// no PSP ainda não foi confirmado. "unknown" é um detalhe interno; expor isso confunde.
const StatusProcessing = "processing"

func viewOf(p *payment.Payment) PaymentView {
	return PaymentView{
		ID:             string(p.ID()),
		Status:         publicStatus(p.Status()),
		Amount:         p.Amount().Amount(),
		Currency:       p.Amount().Currency().String(),
		RefundedAmount: p.RefundedAmount().Amount(),
		FailureReason:  p.FailureReason(),
		CreatedAt:      p.CreatedAt().UTC(),
	}
}

func publicStatus(s payment.Status) string {
	if s == payment.StatusUnknown {
		return StatusProcessing
	}
	return s.String()
}
