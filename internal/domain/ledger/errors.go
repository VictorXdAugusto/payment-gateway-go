package ledger

import "errors"

var (
	ErrUnbalanced         = errors.New("lançamentos não fecham: débitos diferentes de créditos")
	ErrInvalidEntry       = errors.New("lançamento inválido")
	ErrInvalidTransaction = errors.New("transação inválida")
	ErrDuplicateReference = errors.New("movimento já lançado (referência repetida)")
	ErrAccountNotFound    = errors.New("conta não encontrada")
	ErrInvalidFee         = errors.New("taxa inválida")
)
