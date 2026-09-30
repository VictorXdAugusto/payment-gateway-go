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
	CreatedAt      time.Time `json:"created_at"`
}

func viewOf(p *payment.Payment) PaymentView {
	return PaymentView{
		ID:             string(p.ID()),
		Status:         p.Status().String(),
		Amount:         p.Amount().Amount(),
		Currency:       p.Amount().Currency().String(),
		RefundedAmount: p.RefundedAmount().Amount(),
		CreatedAt:      p.CreatedAt().UTC(),
	}
}
