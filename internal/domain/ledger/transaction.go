package ledger

import (
	"fmt"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
)

type (
	TransactionID string
	Kind          string
)

const (
	KindCapture Kind = "capture"
	KindRefund  Kind = "refund"
)

func (k Kind) IsValid() bool { return k == KindCapture || k == KindRefund }

// Entry é uma perna do movimento: um valor positivo em uma conta, para um lado.
type Entry struct {
	Account   Account
	Direction Direction
	Amount    money.Money
}

// Transaction é um movimento completo e IMUTÁVEL. Só existe se os débitos igualarem os créditos.
type Transaction struct {
	id        TransactionID
	reference string // chave de unicidade do movimento: garante que ele só é lançado uma vez
	kind      Kind
	paymentID string
	currency  money.Currency
	entries   []Entry
	createdAt time.Time
}

// NewTransaction valida todos os invariantes antes de devolver a transação:
//   - pelo menos dois lançamentos, todos com valor positivo;
//   - moeda única, igual à moeda de cada conta;
//   - soma dos débitos == soma dos créditos.
func NewTransaction(id TransactionID, reference string, kind Kind, paymentID string, entries []Entry, now time.Time) (Transaction, error) {
	switch {
	case id == "":
		return Transaction{}, fmt.Errorf("%w: id ausente", ErrInvalidTransaction)
	case reference == "":
		return Transaction{}, fmt.Errorf("%w: referência ausente", ErrInvalidTransaction)
	case !kind.IsValid():
		return Transaction{}, fmt.Errorf("%w: tipo %q", ErrInvalidTransaction, kind)
	case paymentID == "":
		return Transaction{}, fmt.Errorf("%w: payment_id ausente", ErrInvalidTransaction)
	case len(entries) < 2:
		return Transaction{}, fmt.Errorf("%w: precisa de ao menos 2 lançamentos", ErrInvalidTransaction)
	}

	currency := entries[0].Amount.Currency()
	debits, credits := zero(currency), zero(currency)

	for i, e := range entries {
		if err := validateEntry(e, currency); err != nil {
			return Transaction{}, fmt.Errorf("lançamento %d: %w", i, err)
		}
		var err error
		if e.Direction == Debit {
			debits, err = debits.Add(e.Amount)
		} else {
			credits, err = credits.Add(e.Amount)
		}
		if err != nil {
			return Transaction{}, fmt.Errorf("lançamento %d: %w", i, err)
		}
	}

	if !debits.Equal(credits) {
		return Transaction{}, fmt.Errorf("%w: débitos %s, créditos %s", ErrUnbalanced, debits, credits)
	}

	return Transaction{
		id: id, reference: reference, kind: kind, paymentID: paymentID, currency: currency,
		entries: append([]Entry(nil), entries...), createdAt: now,
	}, nil
}

func validateEntry(e Entry, currency money.Currency) error {
	switch {
	case !e.Direction.IsValid():
		return fmt.Errorf("%w: direção %q", ErrInvalidEntry, e.Direction)
	case !e.Account.Type.IsValid() || e.Account.ID == "":
		return fmt.Errorf("%w: conta inválida", ErrInvalidEntry)
	case !e.Amount.HasCurrency() || !e.Amount.IsPositive():
		return fmt.Errorf("%w: valor %s (deve ser positivo)", ErrInvalidEntry, e.Amount)
	case e.Amount.Currency() != currency:
		return fmt.Errorf("%w: moeda %s numa transação em %s", ErrInvalidEntry, e.Amount.Currency(), currency)
	case e.Account.Currency != currency:
		return fmt.Errorf("%w: conta %s é %s, lançamento é %s", ErrInvalidEntry, e.Account.ID, e.Account.Currency, currency)
	}
	return nil
}

func zero(c money.Currency) money.Money {
	m, _ := money.New(0, c)
	return m
}

func (t Transaction) ID() TransactionID        { return t.id }
func (t Transaction) Reference() string        { return t.reference }
func (t Transaction) Kind() Kind               { return t.kind }
func (t Transaction) PaymentID() string        { return t.paymentID }
func (t Transaction) Currency() money.Currency { return t.currency }
func (t Transaction) CreatedAt() time.Time     { return t.createdAt }

// Entries devolve uma cópia: quem recebe não consegue alterar a transação por dentro.
func (t Transaction) Entries() []Entry { return append([]Entry(nil), t.entries...) }
