package payment_test

import (
	"errors"
	"testing"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
)

func TestNew(t *testing.T) {
	usd, _ := money.New(500, money.USD)
	zero, _ := money.New(0, money.BRL)
	neg, _ := money.New(-1, money.BRL)

	tests := []struct {
		name       string
		id         payment.ID
		merchant   payment.MerchantID
		amount     money.Money
		wantErr    error
		wantStatus payment.Status
	}{
		{"válido", "p1", "m1", brl(t, 1000), nil, payment.StatusCreated},
		{"outra moeda", "p1", "m1", usd, nil, payment.StatusCreated},
		{"sem id", "", "m1", brl(t, 1000), payment.ErrMissingField, ""},
		{"sem merchant", "p1", "", brl(t, 1000), payment.ErrMissingField, ""},
		{"valor zero", "p1", "m1", zero, payment.ErrInvalidAmount, ""},
		{"valor negativo", "p1", "m1", neg, payment.ErrInvalidAmount, ""},
		{"Money{} sem moeda", "p1", "m1", money.Money{}, payment.ErrMissingField, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := payment.New(tt.id, tt.merchant, tt.amount, t0)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err == nil {
				if p.Status() != tt.wantStatus || !p.RefundedAmount().IsZero() {
					t.Errorf("estado inicial errado: %s, estornado %s", p.Status(), p.RefundedAmount())
				}
				if !p.CreatedAt().Equal(t0) || !p.UpdatedAt().Equal(t0) {
					t.Error("timestamps iniciais devem ser `now`")
				}
			}
		})
	}
}

func TestLifecycle_HappyPath(t *testing.T) {
	p := newPayment(t, 10000)
	later := t0.Add(time.Minute)

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(p.Authorize("psp_42", later))
	must(p.Capture(later))
	must(p.Refund(brl(t, 4000), later))
	if p.Status() != payment.StatusPartiallyRefunded {
		t.Fatalf("status = %s", p.Status())
	}
	if got := p.RefundableAmount().Amount(); got != 6000 {
		t.Errorf("restante = %d, want 6000", got)
	}
	must(p.Refund(brl(t, 6000), later))
	if p.Status() != payment.StatusRefunded || !p.RefundableAmount().IsZero() {
		t.Fatalf("status = %s, restante = %s", p.Status(), p.RefundableAmount())
	}

	if p.PSPReference() != "psp_42" || !p.UpdatedAt().Equal(later) || !p.CreatedAt().Equal(t0) {
		t.Errorf("metadados errados: %q, %v, %v", p.PSPReference(), p.UpdatedAt(), p.CreatedAt())
	}

	wantTypes := []payment.EventType{
		payment.EventCreated, payment.EventAuthorized, payment.EventCaptured,
		payment.EventRefunded, payment.EventRefunded,
	}
	events := p.PullEvents()
	if len(events) != len(wantTypes) {
		t.Fatalf("%d eventos, want %d", len(events), len(wantTypes))
	}
	for i, e := range events {
		if e.Type != wantTypes[i] || e.PaymentID != "pay_1" || e.MerchantID != "mer_1" {
			t.Errorf("evento %d = %+v", i, e)
		}
	}
	if events[3].Amount.Amount() != 4000 || events[4].Amount.Amount() != 6000 {
		t.Error("cada evento de estorno carrega o valor DAQUELE estorno")
	}
	if len(p.PullEvents()) != 0 {
		t.Error("PullEvents deve zerar a fila")
	}
}

func TestVoidAndFailPaths(t *testing.T) {
	v := inState(t, payment.StatusAuthorized)
	if err := v.Void(t0); err != nil || v.Status() != payment.StatusVoided {
		t.Fatalf("void: %v / %s", err, v.Status())
	}

	f := newPayment(t, 100)
	if err := f.Fail("saldo insuficiente", t0); err != nil {
		t.Fatal(err)
	}
	if f.FailureReason() != "saldo insuficiente" {
		t.Errorf("reason = %q", f.FailureReason())
	}
	evs := f.PullEvents()
	if last := evs[len(evs)-1]; last.Type != payment.EventFailed || last.Reason != "saldo insuficiente" {
		t.Errorf("evento de falha = %+v", last)
	}
}

// O caso que justifica o estado Unknown: PSP deu timeout, depois a reconciliação descobre o desfecho.
func TestUnknown_ResolvesEitherWay(t *testing.T) {
	a := inState(t, payment.StatusUnknown)
	if err := a.Authorize("psp_late", t0); err != nil || a.Status() != payment.StatusAuthorized {
		t.Errorf("unknown -> authorized: %v / %s", err, a.Status())
	}

	f := inState(t, payment.StatusUnknown)
	if err := f.Fail("psp não encontrou a transação", t0); err != nil || f.Status() != payment.StatusFailed {
		t.Errorf("unknown -> failed: %v / %s", err, f.Status())
	}

	u := inState(t, payment.StatusUnknown)
	for _, ev := range u.PullEvents() {
		if ev.Type != payment.EventCreated {
			t.Errorf("unknown não deve emitir evento além do created, veio %s", ev.Type)
		}
	}
}

// Cada operação só é válida a partir de certos estados; nos demais deve falhar com
// TransitionError E deixar o agregado intacto (estado, timestamps, eventos).
func TestOperations_RejectInvalidStates(t *testing.T) {
	ops := map[string]struct {
		run   func(*payment.Payment) error
		valid []payment.Status
	}{
		"authorize":    {func(p *payment.Payment) error { return p.Authorize("psp_x", t0.Add(time.Hour)) }, []payment.Status{payment.StatusCreated, payment.StatusUnknown}},
		"fail":         {func(p *payment.Payment) error { return p.Fail("x", t0.Add(time.Hour)) }, []payment.Status{payment.StatusCreated, payment.StatusUnknown}},
		"mark_unknown": {func(p *payment.Payment) error { return p.MarkUnknown(t0.Add(time.Hour)) }, []payment.Status{payment.StatusCreated}},
		"capture":      {func(p *payment.Payment) error { return p.Capture(t0.Add(time.Hour)) }, []payment.Status{payment.StatusAuthorized}},
		"void":         {func(p *payment.Payment) error { return p.Void(t0.Add(time.Hour)) }, []payment.Status{payment.StatusAuthorized}},
		"refund": {func(p *payment.Payment) error { return p.Refund(brl(t, 1), t0.Add(time.Hour)) },
			[]payment.Status{payment.StatusCaptured, payment.StatusPartiallyRefunded}},
	}

	for name, op := range ops {
		for _, s := range payment.AllStatuses() {
			t.Run(name+"/"+string(s), func(t *testing.T) {
				p := inState(t, s)
				before := p.Snapshot()
				p.PullEvents()

				err := op.run(p)

				if contains(op.valid, s) {
					if err != nil {
						t.Fatalf("deveria ser válido em %s: %v", s, err)
					}
					return
				}
				var te *payment.TransitionError
				if !errors.Is(err, payment.ErrInvalidTransition) || !errors.As(err, &te) {
					t.Fatalf("err = %v, want TransitionError", err)
				}
				if te.From != s || te.Operation != name {
					t.Errorf("erro descreve mal a falha: %+v", te)
				}
				if p.Snapshot() != before {
					t.Errorf("operação inválida alterou o agregado:\nantes  %+v\ndepois %+v", before, p.Snapshot())
				}
				if len(p.PullEvents()) != 0 {
					t.Error("operação inválida não pode gerar evento")
				}
			})
		}
	}
}

// Argumento inválido depois de transição válida também não pode deixar o agregado pela metade.
func TestAuthorize_EmptyReferenceLeavesStateUntouched(t *testing.T) {
	p := newPayment(t, 500)
	before := p.Snapshot()

	if err := p.Authorize("", t0.Add(time.Hour)); !errors.Is(err, payment.ErrMissingField) {
		t.Fatalf("err = %v", err)
	}
	if p.Snapshot() != before {
		t.Errorf("Authorize com erro alterou o estado: %+v", p.Snapshot())
	}
}

func TestRefund_Validation(t *testing.T) {
	usd, _ := money.New(100, money.USD)
	zero, _ := money.New(0, money.BRL)
	neg, _ := money.New(-100, money.BRL)

	tests := []struct {
		name    string
		amount  money.Money
		wantErr error
	}{
		{"zero", zero, payment.ErrInvalidAmount},
		{"negativo", neg, payment.ErrInvalidAmount},
		{"maior que o capturado", brl(t, 10001), payment.ErrRefundExceedsCaptured},
		{"outra moeda", usd, money.ErrCurrencyMismatch},
		{"exatamente o total", brl(t, 10000), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := inState(t, payment.StatusCaptured)
			before := p.Snapshot()

			err := p.Refund(tt.amount, t0.Add(time.Hour))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err != nil && p.Snapshot() != before {
				t.Errorf("estorno inválido alterou o agregado")
			}
		})
	}
}

func TestRefund_CannotExceedAcrossMultipleRefunds(t *testing.T) {
	p := inState(t, payment.StatusPartiallyRefunded) // R$ 25,00 já estornados de R$ 100,00
	if err := p.Refund(brl(t, 7501), t0); !errors.Is(err, payment.ErrRefundExceedsCaptured) {
		t.Fatalf("err = %v", err)
	}
	if err := p.Refund(brl(t, 7500), t0); err != nil {
		t.Fatal(err)
	}
	if p.Status() != payment.StatusRefunded {
		t.Errorf("status = %s", p.Status())
	}
}

func TestRestore(t *testing.T) {
	good := inState(t, payment.StatusPartiallyRefunded).Snapshot()

	t.Run("round trip preserva tudo", func(t *testing.T) {
		p, err := payment.Restore(good)
		if err != nil {
			t.Fatal(err)
		}
		if p.Snapshot() != good {
			t.Errorf("snapshot mudou no round trip")
		}
		if len(p.PullEvents()) != 0 {
			t.Error("Restore não deve gerar eventos")
		}
		if err := p.Refund(brl(t, 7500), t0); err != nil {
			t.Errorf("agregado restaurado deve continuar operável: %v", err)
		}
	})

	corrupt := map[string]func(*payment.Snapshot){
		"status desconhecido":         func(s *payment.Snapshot) { s.Status = "banana" },
		"sem id":                      func(s *payment.Snapshot) { s.ID = "" },
		"valor zero":                  func(s *payment.Snapshot) { s.Amount = brl(t, 0) },
		"estornado maior que o valor": func(s *payment.Snapshot) { s.Refunded = brl(t, 10001) },
		"refunded sem estorno total":  func(s *payment.Snapshot) { s.Status = payment.StatusRefunded },
		"parcial sem estorno":         func(s *payment.Snapshot) { s.Refunded = brl(t, 0) },
		"parcial com estorno total":   func(s *payment.Snapshot) { s.Refunded = brl(t, 10000) },
		"captured com estorno":        func(s *payment.Snapshot) { s.Status = payment.StatusCaptured },
		"authorized sem psp ref": func(s *payment.Snapshot) {
			s.Status = payment.StatusAuthorized
			s.Refunded = brl(t, 0)
			s.PSPReference = ""
		},
		"estornado em outra moeda": func(s *payment.Snapshot) {
			usd, _ := money.New(2500, money.USD)
			s.Refunded = usd
		},
	}
	for name, mutate := range corrupt {
		t.Run("rejeita "+name, func(t *testing.T) {
			s := good
			mutate(&s)
			if _, err := payment.Restore(s); err == nil {
				t.Error("deveria rejeitar snapshot corrompido")
			}
		})
	}
}
