package ledger

import (
	"context"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
)

// Balance é o saldo de uma conta DERIVADO da soma dos lançamentos. Não existe coluna
// "saldo" que possa ser atualizada por engano: o ledger é a única fonte da verdade.
type Balance struct {
	Account Account
	Debits  money.Money
	Credits money.Money
}

// Amount devolve o saldo pelo lado natural da conta: ativo = débitos - créditos;
// passivo e receita = créditos - débitos. Pode ser negativo (ex.: estornos > vendas).
func (b Balance) Amount() (money.Money, error) {
	if b.Account.Type.NormalSide() == Debit {
		return b.Debits.Sub(b.Credits)
	}
	return b.Credits.Sub(b.Debits)
}

// BalanceOf calcula o saldo de uma conta a partir de lançamentos em memória (útil em testes).
func BalanceOf(account Account, entries []Entry) (Balance, error) {
	b := Balance{Account: account, Debits: zero(account.Currency), Credits: zero(account.Currency)}
	var err error
	for _, e := range entries {
		if e.Account.ID != account.ID {
			continue
		}
		if e.Direction == Debit {
			b.Debits, err = b.Debits.Add(e.Amount)
		} else {
			b.Credits, err = b.Credits.Add(e.Amount)
		}
		if err != nil {
			return Balance{}, err
		}
	}
	return b, nil
}

// Repository é a porta de persistência do ledger; a implementação vive em infrastructure.
type Repository interface {
	// Post grava a transação inteira ou nada. Devolve ErrDuplicateReference se o
	// movimento já tinha sido lançado (o chamador trata isso como sucesso idempotente).
	Post(ctx context.Context, tx Transaction) error

	// Balance devolve o saldo derivado da conta, ou ErrAccountNotFound.
	Balance(ctx context.Context, id AccountID) (Balance, error)
}
