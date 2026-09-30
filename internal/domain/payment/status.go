// Package payment contém o agregado Payment e a sua máquina de estados.
// É regra de negócio pura: não importa banco, HTTP nem relógio.
package payment

// Status é o que o gateway SABE sobre o pagamento neste momento.
type Status string

const (
	StatusCreated           Status = "created"
	StatusAuthorized        Status = "authorized"
	StatusCaptured          Status = "captured"
	StatusPartiallyRefunded Status = "partially_refunded"
	StatusRefunded          Status = "refunded"
	StatusVoided            Status = "voided"
	StatusFailed            Status = "failed"

	// StatusUnknown: o PSP não respondeu e não sabemos se a autorização aconteceu.
	// Nunca se assume "falhou": o job de reconciliação descobre e resolve.
	StatusUnknown Status = "unknown"
)

// transitions é a ÚNICA fonte de verdade sobre o que pode virar o quê.
// Estado sem entrada (ou com lista vazia) é terminal.
var transitions = map[Status][]Status{
	StatusCreated:           {StatusAuthorized, StatusFailed, StatusUnknown},
	StatusUnknown:           {StatusAuthorized, StatusFailed},
	StatusAuthorized:        {StatusCaptured, StatusVoided},
	StatusCaptured:          {StatusPartiallyRefunded, StatusRefunded},
	StatusPartiallyRefunded: {StatusPartiallyRefunded, StatusRefunded},
	StatusRefunded:          nil,
	StatusVoided:            nil,
	StatusFailed:            nil,
}

// AllStatuses lista todos os estados conhecidos (usado por testes e validações).
func AllStatuses() []Status {
	out := make([]Status, 0, len(transitions))
	for s := range transitions {
		out = append(out, s)
	}
	return out
}

func (s Status) IsValid() bool {
	_, ok := transitions[s]
	return ok
}

func (s Status) CanTransitionTo(to Status) bool {
	for _, allowed := range transitions[s] {
		if allowed == to {
			return true
		}
	}
	return false
}

// IsTerminal indica que nada mais muda neste pagamento.
func (s Status) IsTerminal() bool {
	return s.IsValid() && len(transitions[s]) == 0
}

func (s Status) String() string { return string(s) }
