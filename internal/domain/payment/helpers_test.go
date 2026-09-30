package payment_test

import (
	"testing"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
)

var t0 = time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)

func brl(t testing.TB, cents int64) money.Money {
	t.Helper()
	m, err := money.New(cents, money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func newPayment(t testing.TB, cents int64) *payment.Payment {
	t.Helper()
	p, err := payment.New("pay_1", "mer_1", brl(t, cents), t0)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// inState leva um pagamento novo (R$ 100,00) até o estado desejado usando só a API pública.
func inState(t testing.TB, s payment.Status) *payment.Payment {
	t.Helper()
	p := newPayment(t, 10000)
	step := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("preparando estado %s: %v", s, err)
		}
	}
	switch s {
	case payment.StatusCreated:
	case payment.StatusUnknown:
		step(p.MarkUnknown(t0))
	case payment.StatusFailed:
		step(p.Fail("cartão recusado", t0))
	case payment.StatusAuthorized:
		step(p.Authorize("psp_1", t0))
	case payment.StatusVoided:
		step(p.Authorize("psp_1", t0))
		step(p.Void(t0))
	case payment.StatusCaptured:
		step(p.Authorize("psp_1", t0))
		step(p.Capture(t0))
	case payment.StatusPartiallyRefunded:
		step(p.Authorize("psp_1", t0))
		step(p.Capture(t0))
		step(p.Refund(brl(t, 2500), t0))
	case payment.StatusRefunded:
		step(p.Authorize("psp_1", t0))
		step(p.Capture(t0))
		step(p.Refund(brl(t, 10000), t0))
	default:
		t.Fatalf("estado %s sem preparação no teste", s)
	}
	if p.Status() != s {
		t.Fatalf("preparação terminou em %s, queria %s", p.Status(), s)
	}
	return p
}
