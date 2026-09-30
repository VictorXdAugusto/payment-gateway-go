// Package money implementa valores monetários seguros: inteiros na menor unidade da moeda
// (centavos), com aritmética que nunca perde nem inventa dinheiro.
package money

import (
	"fmt"
	"strings"
)

// Currency é um código ISO 4217 suportado.
type Currency string

const (
	BRL Currency = "BRL"
	USD Currency = "USD"
	EUR Currency = "EUR"
	JPY Currency = "JPY"
)

// exponents guarda quantas casas decimais cada moeda tem (BRL: 2, JPY: 0).
var exponents = map[Currency]int{
	BRL: 2,
	USD: 2,
	EUR: 2,
	JPY: 0,
}

// ParseCurrency valida um código vindo de fora (ex.: JSON da API).
func ParseCurrency(code string) (Currency, error) {
	c := Currency(strings.ToUpper(strings.TrimSpace(code)))
	if !c.IsValid() {
		return "", fmt.Errorf("%w: %q", ErrUnsupportedCurrency, code)
	}
	return c, nil
}

func (c Currency) IsValid() bool {
	_, ok := exponents[c]
	return ok
}

// Exponent devolve o número de casas decimais da moeda (0 se a moeda for desconhecida).
func (c Currency) Exponent() int { return exponents[c] }

func (c Currency) String() string { return string(c) }
