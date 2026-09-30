package ledger

import (
	"fmt"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
)

const maxBasisPoints = 10000 // 100%

// Capture monta o movimento de uma captura: o PSP passa a nos dever o valor cheio;
// devemos ao lojista o valor líquido; a taxa vira receita nossa.
//
//	D psp_clearing        100,00
//	  C merchant:balance          97,10
//	  C fee_revenue                2,90
//
// feeBasisPoints: 290 = 2,90%. O arredondamento da taxa usa Allocate, então
// líquido + taxa é SEMPRE o valor cheio, e o centavo que sobra fica com o gateway.
func Capture(id TransactionID, paymentID, merchantID string, amount money.Money, feeBasisPoints int64, now time.Time) (Transaction, error) {
	if merchantID == "" {
		return Transaction{}, fmt.Errorf("%w: merchant_id ausente", ErrInvalidTransaction)
	}
	if !amount.IsPositive() {
		return Transaction{}, fmt.Errorf("%w: captura de %s", ErrInvalidTransaction, amount)
	}
	fee, net, err := splitFee(amount, feeBasisPoints)
	if err != nil {
		return Transaction{}, err
	}

	c := amount.Currency()
	entries := []Entry{{PSPClearing(c), Debit, amount}}
	if net.IsPositive() {
		entries = append(entries, Entry{MerchantBalance(merchantID, c), Credit, net})
	}
	if fee.IsPositive() {
		entries = append(entries, Entry{FeeRevenue(c), Credit, fee})
	}

	return NewTransaction(id, "capture:"+paymentID, KindCapture, paymentID, entries, now)
}

// Refund monta o movimento de um estorno: devolvemos o valor ao cliente pelo PSP e
// reduzimos o que devemos ao lojista. A taxa NÃO é devolvida (política do case).
//
//	D merchant:balance    40,00
//	  C psp_clearing             40,00
//
// refundID identifica ESTE estorno (um pagamento pode ter vários).
func Refund(id TransactionID, refundID, paymentID, merchantID string, amount money.Money, now time.Time) (Transaction, error) {
	if merchantID == "" {
		return Transaction{}, fmt.Errorf("%w: merchant_id ausente", ErrInvalidTransaction)
	}
	if refundID == "" {
		return Transaction{}, fmt.Errorf("%w: refund_id ausente", ErrInvalidTransaction)
	}
	if !amount.IsPositive() {
		return Transaction{}, fmt.Errorf("%w: estorno de %s", ErrInvalidTransaction, amount)
	}
	c := amount.Currency()
	entries := []Entry{
		{MerchantBalance(merchantID, c), Debit, amount},
		{PSPClearing(c), Credit, amount},
	}
	return NewTransaction(id, "refund:"+refundID, KindRefund, paymentID, entries, now)
}

// splitFee separa taxa e líquido sem perder centavo.
func splitFee(amount money.Money, bps int64) (fee, net money.Money, err error) {
	if bps < 0 || bps > maxBasisPoints {
		return money.Money{}, money.Money{}, fmt.Errorf("%w: %d bps", ErrInvalidFee, bps)
	}
	c := amount.Currency()
	switch bps {
	case 0:
		return zero(c), amount, nil
	case maxBasisPoints:
		return amount, zero(c), nil
	}
	// Allocate exige proporções > 0, por isso os extremos são tratados acima.
	parts, err := amount.Allocate(bps, maxBasisPoints-bps)
	if err != nil {
		return money.Money{}, money.Money{}, err
	}
	return parts[0], parts[1], nil
}
