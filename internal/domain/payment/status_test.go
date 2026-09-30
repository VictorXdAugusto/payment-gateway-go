package payment_test

import (
	"testing"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
)

// allowed é a especificação escrita À MÃO, independente da tabela do código.
// Se alguém mexer na tabela sem intenção, este teste quebra.
var allowed = map[payment.Status][]payment.Status{
	payment.StatusCreated:           {payment.StatusAuthorized, payment.StatusFailed, payment.StatusUnknown},
	payment.StatusUnknown:           {payment.StatusAuthorized, payment.StatusFailed},
	payment.StatusAuthorized:        {payment.StatusCaptured, payment.StatusVoided},
	payment.StatusCaptured:          {payment.StatusPartiallyRefunded, payment.StatusRefunded},
	payment.StatusPartiallyRefunded: {payment.StatusPartiallyRefunded, payment.StatusRefunded},
}

func contains(list []payment.Status, s payment.Status) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// TestTransitionMatrix confere TODOS os pares (origem, destino): o que é permitido passa,
// todo o resto é rejeitado. Testar só os caminhos felizes deixa buraco.
func TestTransitionMatrix(t *testing.T) {
	all := payment.AllStatuses()
	if len(all) != 8 {
		t.Fatalf("esperava 8 estados, tem %d: a matriz precisa ser revisada", len(all))
	}
	for _, from := range all {
		for _, to := range all {
			want := contains(allowed[from], to)
			if got := from.CanTransitionTo(to); got != want {
				t.Errorf("%s -> %s: got %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestTerminalStates(t *testing.T) {
	terminal := map[payment.Status]bool{
		payment.StatusRefunded: true, payment.StatusVoided: true, payment.StatusFailed: true,
	}
	for _, s := range payment.AllStatuses() {
		if got := s.IsTerminal(); got != terminal[s] {
			t.Errorf("%s.IsTerminal() = %v, want %v", s, got, terminal[s])
		}
	}
	if payment.Status("banana").IsTerminal() || payment.Status("banana").IsValid() {
		t.Error("status desconhecido não pode ser válido nem terminal")
	}
}
