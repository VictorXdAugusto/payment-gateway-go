package usecase

import (
	"context"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
)

type GetPayment struct {
	payments payment.Repository
}

func NewGetPayment(payments payment.Repository) *GetPayment { return &GetPayment{payments: payments} }

// Execute devolve o pagamento SE ele pertencer ao lojista. Id de outro lojista é "não encontrado".
func (uc *GetPayment) Execute(ctx context.Context, merchantID, id string) (PaymentView, error) {
	p, err := uc.payments.Get(ctx, payment.MerchantID(merchantID), payment.ID(id))
	if err != nil {
		return PaymentView{}, err
	}
	return viewOf(p), nil
}
