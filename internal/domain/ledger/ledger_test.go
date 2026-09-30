package ledger_test

import (
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/ledger"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
)

var now = time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)

func brl(t testing.TB, cents int64) money.Money {
	t.Helper()
	m, err := money.New(cents, money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func amountOf(t testing.TB, tx ledger.Transaction, acc ledger.Account) (debit, credit int64) {
	t.Helper()
	b, err := ledger.BalanceOf(acc, tx.Entries())
	if err != nil {
		t.Fatal(err)
	}
	return b.Debits.Amount(), b.Credits.Amount()
}

func TestNewTransaction_Validation(t *testing.T) {
	psp, mer := ledger.PSPClearing(money.BRL), ledger.MerchantBalance("m1", money.BRL)
	usdPSP := ledger.PSPClearing(money.USD)
	usd, _ := money.New(100, money.USD)

	tests := []struct {
		name    string
		entries []ledger.Entry
		wantErr error
	}{
		{"balanceada", []ledger.Entry{{psp, ledger.Debit, brl(t, 100)}, {mer, ledger.Credit, brl(t, 100)}}, nil},
		{"balanceada com 3 pernas", []ledger.Entry{{psp, ledger.Debit, brl(t, 100)}, {mer, ledger.Credit, brl(t, 97)}, {ledger.FeeRevenue(money.BRL), ledger.Credit, brl(t, 3)}}, nil},
		{"débito maior que crédito", []ledger.Entry{{psp, ledger.Debit, brl(t, 101)}, {mer, ledger.Credit, brl(t, 100)}}, ledger.ErrUnbalanced},
		{"só débitos", []ledger.Entry{{psp, ledger.Debit, brl(t, 100)}, {mer, ledger.Debit, brl(t, 100)}}, ledger.ErrUnbalanced},
		{"uma perna só", []ledger.Entry{{psp, ledger.Debit, brl(t, 100)}}, ledger.ErrInvalidTransaction},
		{"sem pernas", nil, ledger.ErrInvalidTransaction},
		{"valor zero", []ledger.Entry{{psp, ledger.Debit, brl(t, 0)}, {mer, ledger.Credit, brl(t, 0)}}, ledger.ErrInvalidEntry},
		{"valor negativo", []ledger.Entry{{psp, ledger.Debit, brl(t, -5)}, {mer, ledger.Credit, brl(t, -5)}}, ledger.ErrInvalidEntry},
		{"direção inválida", []ledger.Entry{{psp, "sideways", brl(t, 100)}, {mer, ledger.Credit, brl(t, 100)}}, ledger.ErrInvalidEntry},
		{"moedas misturadas", []ledger.Entry{{psp, ledger.Debit, brl(t, 100)}, {usdPSP, ledger.Credit, usd}}, ledger.ErrInvalidEntry},
		{"valor em USD numa conta BRL", []ledger.Entry{{psp, ledger.Debit, usd}, {usdPSP, ledger.Credit, usd}}, ledger.ErrInvalidEntry},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ledger.NewTransaction("t1", "capture:p1", ledger.KindCapture, "p1", tt.entries, now)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewTransaction_RequiredFields(t *testing.T) {
	psp, mer := ledger.PSPClearing(money.BRL), ledger.MerchantBalance("m1", money.BRL)
	ok := []ledger.Entry{{psp, ledger.Debit, brl(t, 1)}, {mer, ledger.Credit, brl(t, 1)}}

	cases := map[string]func() error{
		"sem id":        func() error { _, e := ledger.NewTransaction("", "r", ledger.KindCapture, "p", ok, now); return e },
		"sem reference": func() error { _, e := ledger.NewTransaction("t", "", ledger.KindCapture, "p", ok, now); return e },
		"kind inválido": func() error { _, e := ledger.NewTransaction("t", "r", "x", "p", ok, now); return e },
		"sem payment":   func() error { _, e := ledger.NewTransaction("t", "r", ledger.KindCapture, "", ok, now); return e },
	}
	for name, f := range cases {
		if err := f(); !errors.Is(err, ledger.ErrInvalidTransaction) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestTransaction_IsImmutable(t *testing.T) {
	psp, mer := ledger.PSPClearing(money.BRL), ledger.MerchantBalance("m1", money.BRL)
	src := []ledger.Entry{{psp, ledger.Debit, brl(t, 100)}, {mer, ledger.Credit, brl(t, 100)}}
	tx, err := ledger.NewTransaction("t", "r", ledger.KindCapture, "p", src, now)
	if err != nil {
		t.Fatal(err)
	}

	src[0].Amount = brl(t, 999) // mexer no slice original não pode afetar a transação
	got := tx.Entries()
	got[1].Amount = brl(t, 555) // nem mexer na cópia devolvida

	for _, e := range tx.Entries() {
		if e.Amount.Amount() != 100 {
			t.Fatalf("transação foi alterada por fora: %+v", tx.Entries())
		}
	}
}

func TestCapture_SplitsFeeAndNet(t *testing.T) {
	tx, err := ledger.Capture("t1", "pay_1", "m1", brl(t, 10000), 290, now)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Reference() != "capture:pay_1" || tx.Kind() != ledger.KindCapture {
		t.Errorf("ref/kind = %s/%s", tx.Reference(), tx.Kind())
	}

	if d, _ := amountOf(t, tx, ledger.PSPClearing(money.BRL)); d != 10000 {
		t.Errorf("psp_clearing débito = %d", d)
	}
	if _, c := amountOf(t, tx, ledger.MerchantBalance("m1", money.BRL)); c != 9710 {
		t.Errorf("líquido do lojista = %d, want 9710", c)
	}
	if _, c := amountOf(t, tx, ledger.FeeRevenue(money.BRL)); c != 290 {
		t.Errorf("taxa = %d, want 290", c)
	}
}

func TestCapture_FeeRoundingKeepsTheCent(t *testing.T) {
	// 2,9% de R$ 0,33 = 0,00957: o centavo arredonda a favor do gateway, sem sumir.
	tx, err := ledger.Capture("t1", "p", "m1", brl(t, 33), 290, now)
	if err != nil {
		t.Fatal(err)
	}
	_, fee := amountOf(t, tx, ledger.FeeRevenue(money.BRL))
	_, net := amountOf(t, tx, ledger.MerchantBalance("m1", money.BRL))
	if fee+net != 33 || fee != 1 {
		t.Errorf("fee=%d net=%d, want soma 33 e taxa 1", fee, net)
	}
}

func TestCapture_FeeEdges(t *testing.T) {
	tests := []struct {
		name          string
		bps           int64
		wantFee, want int64
		wantErr       error
		wantLegs      int
	}{
		{"sem taxa", 0, 0, 5000, nil, 2},
		{"taxa de 100%", 10000, 5000, 0, nil, 2},
		{"taxa negativa", -1, 0, 0, ledger.ErrInvalidFee, 0},
		{"taxa acima de 100%", 10001, 0, 0, ledger.ErrInvalidFee, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx, err := ledger.Capture("t", "p", "m1", brl(t, 5000), tt.bps, now)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v", err)
			}
			if err != nil {
				return
			}
			if len(tx.Entries()) != tt.wantLegs {
				t.Errorf("pernas = %d, want %d", len(tx.Entries()), tt.wantLegs)
			}
			_, fee := amountOf(t, tx, ledger.FeeRevenue(money.BRL))
			_, net := amountOf(t, tx, ledger.MerchantBalance("m1", money.BRL))
			if fee != tt.wantFee || net != tt.want {
				t.Errorf("fee=%d net=%d", fee, net)
			}
		})
	}
}

func TestCapture_And_Refund_RejectNonPositive(t *testing.T) {
	if _, err := ledger.Capture("t", "p", "m", brl(t, 0), 290, now); !errors.Is(err, ledger.ErrInvalidTransaction) {
		t.Errorf("capture zero: %v", err)
	}
	if _, err := ledger.Refund("t", "r1", "p", "m", brl(t, -1), now); !errors.Is(err, ledger.ErrInvalidTransaction) {
		t.Errorf("refund negativo: %v", err)
	}
	if _, err := ledger.Refund("t", "", "p", "m", brl(t, 1), now); !errors.Is(err, ledger.ErrInvalidTransaction) {
		t.Errorf("refund sem id: %v", err)
	}
}

func TestRefund_ReversesTheMerchantLeg(t *testing.T) {
	tx, err := ledger.Refund("t2", "rf_1", "pay_1", "m1", brl(t, 4000), now)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Reference() != "refund:rf_1" {
		t.Errorf("ref = %s", tx.Reference())
	}
	if d, _ := amountOf(t, tx, ledger.MerchantBalance("m1", money.BRL)); d != 4000 {
		t.Errorf("débito no lojista = %d", d)
	}
	if _, c := amountOf(t, tx, ledger.PSPClearing(money.BRL)); c != 4000 {
		t.Errorf("crédito no psp = %d", c)
	}
}

func TestBalance_NormalSides(t *testing.T) {
	capture, _ := ledger.Capture("t1", "p", "m1", brl(t, 10000), 290, now)
	refund, _ := ledger.Refund("t2", "r1", "p", "m1", brl(t, 4000), now)
	all := append(capture.Entries(), refund.Entries()...)

	want := map[ledger.Account]int64{
		ledger.PSPClearing(money.BRL):           6000, // ativo: 10000 - 4000
		ledger.MerchantBalance("m1", money.BRL): 5710, // passivo: 9710 - 4000
		ledger.FeeRevenue(money.BRL):            290,  // receita
	}
	for acc, exp := range want {
		b, err := ledger.BalanceOf(acc, all)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := b.Amount()
		if got.Amount() != exp {
			t.Errorf("%s = %d, want %d", acc.ID, got.Amount(), exp)
		}
	}
}

// Propriedade central: para QUALQUER valor e taxa, o movimento fecha (débitos == créditos)
// e taxa + líquido == valor cheio. Sem exceção, sem centavo perdido.
func TestCapture_AlwaysBalanced_Property(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 20000; i++ {
		amount := rng.Int63n(1_000_000_000) + 1
		bps := rng.Int63n(10001)

		tx, err := ledger.Capture("t", "p", "m", brl(t, amount), bps, now)
		if err != nil {
			t.Fatalf("amount=%d bps=%d: %v", amount, bps, err)
		}
		var deb, cred int64
		for _, e := range tx.Entries() {
			if e.Direction == ledger.Debit {
				deb += e.Amount.Amount()
			} else {
				cred += e.Amount.Amount()
			}
		}
		if deb != amount || cred != amount {
			t.Fatalf("amount=%d bps=%d: débitos=%d créditos=%d", amount, bps, deb, cred)
		}
	}
}

func TestCapture_And_Refund_RejectEmptyMerchant(t *testing.T) {
	if _, err := ledger.Capture("t", "p", "", brl(t, 1000), 290, now); !errors.Is(err, ledger.ErrInvalidTransaction) {
		t.Errorf("capture sem lojista: %v", err)
	}
	if _, err := ledger.Refund("t", "r1", "p", "", brl(t, 1000), now); !errors.Is(err, ledger.ErrInvalidTransaction) {
		t.Errorf("refund sem lojista: %v", err)
	}
}

func TestBalanceOf_RejectsInvalidDirection(t *testing.T) {
	psp := ledger.PSPClearing(money.BRL)
	entries := []ledger.Entry{{Account: psp, Direction: "deibt", Amount: brl(t, 100)}}
	if _, err := ledger.BalanceOf(psp, entries); !errors.Is(err, ledger.ErrInvalidTransaction) {
		t.Fatalf("err = %v, want ErrInvalidTransaction", err)
	}
}
