package ledger

import (
	"context"
	"fmt"
	"time"

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
		if !e.Direction.IsValid() {
			return Balance{}, fmt.Errorf("%w: direção %q", ErrInvalidTransaction, e.Direction)
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

// StatementLine é um lançamento na conta do lojista, como aparece no extrato.
type StatementLine struct {
	EntryID       int64 // cresce com o tempo: serve de cursor de paginação
	TransactionID TransactionID
	Kind          Kind
	PaymentID     string
	Direction     Direction
	Amount        money.Money
	CreatedAt     time.Time
}

// Repository é a porta de persistência do ledger; a implementação vive em infrastructure.
type Repository interface {
	// Post grava a transação inteira ou nada. Se a referência já existe, compara o conteúdo
	// financeiro (tipo, pagamento e pernas) com o que foi gravado:
	//   - idêntico: devolve ErrDuplicateReference (repetição legítima, o chamador trata
	//     como sucesso idempotente);
	//   - diferente: devolve ErrReferenceConflict (a mesma referência com outro valor não
	//     pode ser tratada como sucesso, pois só o movimento original foi contabilizado).
	Post(ctx context.Context, tx Transaction) error

	// Balance devolve o saldo derivado da conta, ou ErrAccountNotFound.
	Balance(ctx context.Context, id AccountID) (Balance, error)

	// MerchantBalances devolve o saldo do lojista em cada moeda que ele já movimentou.
	MerchantBalances(ctx context.Context, merchantID string) ([]Balance, error)

	// MerchantStatement devolve os lançamentos do lojista, do mais novo para o mais antigo,
	// só os de EntryID menor que before (0 = desde o início), no máximo limit.
	MerchantStatement(ctx context.Context, merchantID string, before int64, limit int) ([]StatementLine, error)
}
