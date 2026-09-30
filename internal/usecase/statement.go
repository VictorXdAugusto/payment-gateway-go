package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/ledger"
)

const (
	DefaultStatementLimit = 50
	MaxStatementLimit     = 100
)

// BalanceView é o saldo do lojista em uma moeda: quanto o gateway deve a ele.
type BalanceView struct {
	Currency string `json:"currency"`
	Amount   int64  `json:"amount"` // pode ser negativo (ex.: estornos e taxas maiores que as vendas)
}

// GetBalance devolve os saldos do lojista, derivados do ledger (não existe coluna de saldo).
type GetBalance struct {
	ledger ledger.Repository
}

func NewGetBalance(lg ledger.Repository) *GetBalance { return &GetBalance{ledger: lg} }

func (uc *GetBalance) Execute(ctx context.Context, merchantID string) ([]BalanceView, error) {
	balances, err := uc.ledger.MerchantBalances(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	out := make([]BalanceView, 0, len(balances))
	for _, b := range balances {
		amount, err := b.Amount()
		if err != nil {
			return nil, err
		}
		out = append(out, BalanceView{Currency: amount.Currency().String(), Amount: amount.Amount()})
	}
	return out, nil
}

// StatementLineView é um lançamento do extrato pela ótica do lojista: positivo aumenta o que
// ele tem a receber (venda líquida), negativo diminui (estorno).
type StatementLineView struct {
	ID            int64     `json:"id"`
	Type          string    `json:"type"` // capture | refund
	PaymentID     string    `json:"payment_id"`
	TransactionID string    `json:"transaction_id"`
	Amount        int64     `json:"amount"`
	Currency      string    `json:"currency"`
	CreatedAt     time.Time `json:"created_at"`
}

type StatementPage struct {
	Data       []StatementLineView `json:"data"`
	NextBefore *int64              `json:"next_before"` // passe em ?before= para a página seguinte; null = acabou
}

type GetStatement struct {
	ledger ledger.Repository
}

func NewGetStatement(lg ledger.Repository) *GetStatement { return &GetStatement{ledger: lg} }

// Execute pagina o extrato. limit 0 usa o padrão; acima do máximo ou negativo é entrada inválida.
func (uc *GetStatement) Execute(ctx context.Context, merchantID string, before int64, limit int) (StatementPage, error) {
	switch {
	case limit == 0:
		limit = DefaultStatementLimit
	case limit < 0 || limit > MaxStatementLimit:
		return StatementPage{}, fmt.Errorf("%w: limit deve estar entre 1 e %d", ErrInvalidInput, MaxStatementLimit)
	}
	if before < 0 {
		return StatementPage{}, fmt.Errorf("%w: before inválido", ErrInvalidInput)
	}

	// Pede UMA a mais para saber se existe próxima página sem uma segunda consulta.
	lines, err := uc.ledger.MerchantStatement(ctx, merchantID, before, limit+1)
	if err != nil {
		return StatementPage{}, err
	}
	page := StatementPage{Data: make([]StatementLineView, 0, len(lines))}
	if len(lines) > limit {
		lines = lines[:limit]
		next := lines[len(lines)-1].EntryID
		page.NextBefore = &next
	}
	for _, l := range lines {
		amount := l.Amount.Amount()
		if l.Direction == ledger.Debit { // conta de passivo: débito diminui o saldo do lojista
			amount = -amount
		}
		page.Data = append(page.Data, StatementLineView{
			ID: l.EntryID, Type: string(l.Kind), PaymentID: l.PaymentID, TransactionID: string(l.TransactionID),
			Amount: amount, Currency: l.Amount.Currency().String(), CreatedAt: l.CreatedAt,
		})
	}
	return page, nil
}
