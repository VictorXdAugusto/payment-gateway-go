// Package ledger implementa o livro-razão de partida dobrada: todo movimento de dinheiro
// é uma transação com lançamentos de débito e crédito que sempre fecham em zero.
package ledger

import (
	"fmt"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
)

type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

func (d Direction) IsValid() bool { return d == Debit || d == Credit }

// AccountType define o lado "natural" da conta, ou seja, para que lado o saldo cresce.
type AccountType string

const (
	// Asset: o que temos a receber. Cresce com débito. Ex.: dinheiro que o PSP nos deve.
	Asset AccountType = "asset"
	// Liability: o que devemos. Cresce com crédito. Ex.: saldo dos lojistas.
	Liability AccountType = "liability"
	// Revenue: o que ganhamos. Cresce com crédito. Ex.: taxas cobradas.
	Revenue AccountType = "revenue"
)

func (t AccountType) IsValid() bool { return t == Asset || t == Liability || t == Revenue }

func (t AccountType) NormalSide() Direction {
	if t == Asset {
		return Debit
	}
	return Credit
}

type AccountID string

// Account é uma conta do plano de contas. Cada conta guarda UMA moeda.
type Account struct {
	ID         AccountID
	Type       AccountType
	Currency   money.Currency
	MerchantID string // vazio nas contas do próprio gateway
}

// PSPClearing: valores que o adquirente ainda vai nos repassar (ativo).
func PSPClearing(c money.Currency) Account {
	return Account{ID: AccountID("psp_clearing:" + string(c)), Type: Asset, Currency: c}
}

// FeeRevenue: receita de taxas do gateway.
func FeeRevenue(c money.Currency) Account {
	return Account{ID: AccountID("fee_revenue:" + string(c)), Type: Revenue, Currency: c}
}

// MerchantBalance: o que devemos ao lojista (passivo).
func MerchantBalance(merchantID string, c money.Currency) Account {
	return Account{
		ID:         AccountID(fmt.Sprintf("merchant:%s:%s", merchantID, c)),
		Type:       Liability,
		Currency:   c,
		MerchantID: merchantID,
	}
}
