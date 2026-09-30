package money

import "errors"

var (
	ErrUnsupportedCurrency = errors.New("moeda não suportada")
	ErrCurrencyMismatch    = errors.New("moedas diferentes")
	ErrOverflow            = errors.New("estouro de int64")
	ErrInvalidAllocation   = errors.New("proporções de divisão inválidas")
)
