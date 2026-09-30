package payment_test

import (
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
)

// TestRandomOperationSequences dispara milhares de sequências aleatórias de operações
// (a maioria inválida) e exige que, aconteça o que acontecer:
//   - nenhuma operação entre em pânico;
//   - o agregado sempre passe pela validação do Restore (invariantes de estado);
//   - nunca se estorne mais do que foi capturado;
//   - um estado terminal nunca mude;
//   - só mude de estado por uma seta da tabela.
//
// Semente fixa: se falhar, a falha é reproduzível.
func TestRandomOperationSequences(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	now := t0

	for seq := 0; seq < 3000; seq++ {
		total := int64(rng.Intn(20000) + 1)
		p := newPayment(t, total)

		for step := 0; step < 12; step++ {
			now = now.Add(time.Second)
			from := p.Status()
			before := p.Snapshot()

			var err error
			switch rng.Intn(6) {
			case 0:
				err = p.Authorize("psp_"+string(rune('a'+rng.Intn(26))), now)
			case 1:
				err = p.Fail("motivo", now)
			case 2:
				err = p.MarkUnknown(now)
			case 3:
				err = p.Capture(now)
			case 4:
				err = p.Void(now)
			case 5:
				cents := int64(rng.Intn(int(total)+5000)) - 100 // inclui zero, negativo e acima do total
				m, _ := money.New(cents, money.BRL)
				err = p.Refund(m, now)
			}

			if err != nil {
				if p.Snapshot() != before {
					t.Fatalf("seq %d step %d: operação com erro alterou o agregado (%v)", seq, step, err)
				}
				var te *payment.TransitionError
				if !errors.As(err, &te) &&
					!errors.Is(err, payment.ErrInvalidAmount) &&
					!errors.Is(err, payment.ErrRefundExceedsCaptured) {
					t.Fatalf("seq %d: erro inesperado %v", seq, err)
				}
			}

			to := p.Status()
			if from.IsTerminal() && to != from {
				t.Fatalf("seq %d: estado terminal %s mudou para %s", seq, from, to)
			}
			if to != from && !from.CanTransitionTo(to) {
				t.Fatalf("seq %d: %s -> %s não está na tabela", seq, from, to)
			}
			if _, rerr := payment.Restore(p.Snapshot()); rerr != nil {
				t.Fatalf("seq %d step %d: invariante violado em %s: %v", seq, step, to, rerr)
			}
			if c, _ := p.RefundedAmount().Compare(p.Amount()); c > 0 {
				t.Fatalf("seq %d: estornado %s maior que %s", seq, p.RefundedAmount(), p.Amount())
			}
		}
	}
}
