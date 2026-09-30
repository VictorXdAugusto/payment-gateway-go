package money

import (
	"fmt"
	"math"
	"math/big"
)

// Money é um valor imutável: quantidade inteira na menor unidade da moeda + moeda.
// Nunca use float para dinheiro: 0.1 + 0.2 != 0.3 em ponto flutuante.
//
// O valor zero de Money (Money{}) não tem moeda e serve só como "ausente";
// toda operação entre moedas diferentes falha com ErrCurrencyMismatch.
type Money struct {
	amount   int64
	currency Currency
}

// New cria um Money. O valor é em centavos: New(1050, BRL) é R$ 10,50.
func New(amount int64, currency Currency) (Money, error) {
	if !currency.IsValid() {
		return Money{}, fmt.Errorf("%w: %q", ErrUnsupportedCurrency, currency)
	}
	return Money{amount: amount, currency: currency}, nil
}

func (m Money) Amount() int64      { return m.amount }
func (m Money) Currency() Currency { return m.currency }
func (m Money) IsZero() bool       { return m.amount == 0 }
func (m Money) IsPositive() bool   { return m.amount > 0 }
func (m Money) IsNegative() bool   { return m.amount < 0 }

// HasCurrency diferencia um Money construído de um Money{} (valor zero da struct).
func (m Money) HasCurrency() bool { return m.currency.IsValid() }

func (m Money) Add(o Money) (Money, error) {
	if m.currency != o.currency {
		return Money{}, fmt.Errorf("%w: %s e %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	if (o.amount > 0 && m.amount > math.MaxInt64-o.amount) ||
		(o.amount < 0 && m.amount < math.MinInt64-o.amount) {
		return Money{}, ErrOverflow
	}
	return Money{amount: m.amount + o.amount, currency: m.currency}, nil
}

func (m Money) Sub(o Money) (Money, error) {
	if m.currency != o.currency {
		return Money{}, fmt.Errorf("%w: %s e %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	if (o.amount < 0 && m.amount > math.MaxInt64+o.amount) ||
		(o.amount > 0 && m.amount < math.MinInt64+o.amount) {
		return Money{}, ErrOverflow
	}
	return Money{amount: m.amount - o.amount, currency: m.currency}, nil
}

// Neg inverte o sinal. Falha só para math.MinInt64, cujo oposto não cabe em int64.
func (m Money) Neg() (Money, error) {
	if m.amount == math.MinInt64 {
		return Money{}, ErrOverflow
	}
	return Money{amount: -m.amount, currency: m.currency}, nil
}

// Compare devolve -1, 0 ou 1. Comparar moedas diferentes é erro, nunca "maior" ou "menor".
func (m Money) Compare(o Money) (int, error) {
	if m.currency != o.currency {
		return 0, fmt.Errorf("%w: %s e %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	switch {
	case m.amount < o.amount:
		return -1, nil
	case m.amount > o.amount:
		return 1, nil
	default:
		return 0, nil
	}
}

func (m Money) Equal(o Money) bool { return m == o }

// Allocate divide o valor proporcionalmente a ratios sem perder nem criar centavos:
// a soma das partes é sempre exatamente igual ao total. Os centavos que sobram do
// arredondamento vão, um a um, para as primeiras partes.
//
//	R$ 1,00 dividido em 3 partes iguais -> 0,34 + 0,33 + 0,33
//
// Serve para comissões, splits entre lojistas e parcelamento.
func (m Money) Allocate(ratios ...int64) ([]Money, error) {
	if len(ratios) == 0 {
		return nil, fmt.Errorf("%w: nenhuma proporção", ErrInvalidAllocation)
	}
	if !m.HasCurrency() {
		return nil, ErrUnsupportedCurrency
	}

	totalRatio := new(big.Int)
	for _, r := range ratios {
		if r <= 0 {
			return nil, fmt.Errorf("%w: proporção %d", ErrInvalidAllocation, r)
		}
		totalRatio.Add(totalRatio, big.NewInt(r))
	}

	// big.Int evita overflow em amount*ratio e o caso |MinInt64|.
	abs := new(big.Int).Abs(big.NewInt(m.amount))
	shares := make([]*big.Int, len(ratios))
	distributed := new(big.Int)
	for i, r := range ratios {
		s := new(big.Int).Mul(abs, big.NewInt(r))
		s.Quo(s, totalRatio)
		shares[i] = s
		distributed.Add(distributed, s)
	}

	remainder := new(big.Int).Sub(abs, distributed).Int64() // sempre < len(ratios)
	one := big.NewInt(1)
	for i := int64(0); i < remainder; i++ {
		shares[i].Add(shares[i], one)
	}

	out := make([]Money, len(shares))
	for i, s := range shares {
		if m.amount < 0 {
			s.Neg(s)
		}
		if !s.IsInt64() {
			return nil, ErrOverflow
		}
		out[i] = Money{amount: s.Int64(), currency: m.currency}
	}
	return out, nil
}

// String formata para log e depuração, ex.: "10.50 BRL", "-0.05 USD", "1500 JPY".
func (m Money) String() string {
	abs := uint64(m.amount)
	sign := ""
	if m.amount < 0 {
		sign = "-"
		abs = uint64(-(m.amount + 1)) + 1 // evita overflow no MinInt64
	}

	exp := m.currency.Exponent()
	if exp == 0 {
		return fmt.Sprintf("%s%d %s", sign, abs, m.currency)
	}

	pow := uint64(1)
	for i := 0; i < exp; i++ {
		pow *= 10
	}
	return fmt.Sprintf("%s%d.%0*d %s", sign, abs/pow, exp, abs%pow, m.currency)
}
