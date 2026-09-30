package ledger

import "errors"

var (
	ErrUnbalanced         = errors.New("lançamentos não fecham: débitos diferentes de créditos")
	ErrInvalidEntry       = errors.New("lançamento inválido")
	ErrInvalidTransaction = errors.New("transação inválida")
	ErrDuplicateReference = errors.New("movimento já lançado (referência repetida)")
	ErrReferenceConflict  = errors.New("referência já usada por um movimento diferente")
	ErrAccountMismatch    = errors.New("conta existente diverge do tipo, moeda ou lojista informado")
	ErrAccountNotFound    = errors.New("conta não encontrada")
	ErrInvalidFee         = errors.New("taxa inválida")
)
