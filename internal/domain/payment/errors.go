package payment

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidTransition     = errors.New("transição de estado inválida")
	ErrInvalidAmount         = errors.New("valor inválido")
	ErrRefundExceedsCaptured = errors.New("estorno maior que o valor restante")
	ErrInvalidState          = errors.New("estado inconsistente")
	ErrMissingField          = errors.New("campo obrigatório ausente")
)

// TransitionError diz qual operação tentou ir de qual estado para qual.
// errors.Is(err, ErrInvalidTransition) continua funcionando.
type TransitionError struct {
	Operation string
	From      Status
	To        Status
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("%v: %s (%s -> %s)", ErrInvalidTransition, e.Operation, e.From, e.To)
}

func (e *TransitionError) Is(target error) bool { return target == ErrInvalidTransition }
