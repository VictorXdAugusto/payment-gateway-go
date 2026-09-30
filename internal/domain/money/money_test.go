package money_test

import (
	"errors"
	"math"
	"testing"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
)

func mustNew(t testing.TB, amount int64, c money.Currency) money.Money {
	t.Helper()
	m, err := money.New(amount, c)
	if err != nil {
		t.Fatalf("money.New(%d, %s): %v", amount, c, err)
	}
	return m
}

func TestNew_RejectsUnsupportedCurrency(t *testing.T) {
	_, err := money.New(100, money.Currency("XYZ"))
	if !errors.Is(err, money.ErrUnsupportedCurrency) {
		t.Fatalf("err = %v, want ErrUnsupportedCurrency", err)
	}
}

func TestParseCurrency(t *testing.T) {
	tests := []struct {
		in      string
		want    money.Currency
		wantErr bool
	}{
		{"BRL", money.BRL, false},
		{" usd ", money.USD, false},
		{"jpy", money.JPY, false},
		{"", "", true},
		{"XYZ", "", true},
	}
	for _, tt := range tests {
		got, err := money.ParseCurrency(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseCurrency(%q) = (%q, %v), want (%q, err=%v)", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestAddSub(t *testing.T) {
	tests := []struct {
		name   string
		a, b   int64
		add    int64
		sub    int64
		addErr error
		subErr error
	}{
		{name: "positivos", a: 1000, b: 250, add: 1250, sub: 750},
		{name: "resultado negativo", a: 100, b: 250, add: 350, sub: -150},
		{name: "overflow no add", a: math.MaxInt64, b: 1, addErr: money.ErrOverflow, sub: math.MaxInt64 - 1},
		{name: "overflow negativo no add", a: math.MinInt64, b: -1, addErr: money.ErrOverflow, sub: math.MinInt64 + 1},
		{name: "overflow no sub", a: math.MinInt64, b: 1, add: math.MinInt64 + 1, subErr: money.ErrOverflow},
		{name: "overflow no sub com b negativo", a: math.MaxInt64, b: -1, add: math.MaxInt64 - 1, subErr: money.ErrOverflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := mustNew(t, tt.a, money.BRL), mustNew(t, tt.b, money.BRL)

			sum, err := a.Add(b)
			if !errors.Is(err, tt.addErr) {
				t.Fatalf("Add err = %v, want %v", err, tt.addErr)
			}
			if tt.addErr == nil && sum.Amount() != tt.add {
				t.Errorf("Add = %d, want %d", sum.Amount(), tt.add)
			}

			diff, err := a.Sub(b)
			if !errors.Is(err, tt.subErr) {
				t.Fatalf("Sub err = %v, want %v", err, tt.subErr)
			}
			if tt.subErr == nil && diff.Amount() != tt.sub {
				t.Errorf("Sub = %d, want %d", diff.Amount(), tt.sub)
			}
		})
	}
}

func TestOperationsRejectCurrencyMismatch(t *testing.T) {
	brl, usd := mustNew(t, 100, money.BRL), mustNew(t, 100, money.USD)

	if _, err := brl.Add(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Add err = %v", err)
	}
	if _, err := brl.Sub(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Sub err = %v", err)
	}
	if _, err := brl.Compare(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Compare err = %v", err)
	}
	if brl.Equal(usd) {
		t.Error("100 BRL não pode ser igual a 100 USD")
	}
}

func TestCompare(t *testing.T) {
	a, b := mustNew(t, 100, money.BRL), mustNew(t, 200, money.BRL)
	for _, tt := range []struct {
		x, y money.Money
		want int
	}{{a, b, -1}, {b, a, 1}, {a, a, 0}} {
		got, err := tt.x.Compare(tt.y)
		if err != nil || got != tt.want {
			t.Errorf("Compare(%v, %v) = (%d, %v), want %d", tt.x, tt.y, got, err, tt.want)
		}
	}
}

func TestNeg(t *testing.T) {
	got, err := mustNew(t, 500, money.BRL).Neg()
	if err != nil || got.Amount() != -500 {
		t.Fatalf("Neg = (%v, %v)", got, err)
	}
	if _, err := mustNew(t, math.MinInt64, money.BRL).Neg(); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("Neg(MinInt64) err = %v, want ErrOverflow", err)
	}
}

func TestString(t *testing.T) {
	tests := []struct {
		amount int64
		c      money.Currency
		want   string
	}{
		{1050, money.BRL, "10.50 BRL"},
		{5, money.USD, "0.05 USD"},
		{-5, money.USD, "-0.05 USD"},
		{0, money.EUR, "0.00 EUR"},
		{1500, money.JPY, "1500 JPY"},
		{math.MinInt64, money.BRL, "-92233720368547758.08 BRL"},
	}
	for _, tt := range tests {
		if got := mustNew(t, tt.amount, tt.c).String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}

func amounts(ms []money.Money) []int64 {
	out := make([]int64, len(ms))
	for i, m := range ms {
		out[i] = m.Amount()
	}
	return out
}

func TestAllocate(t *testing.T) {
	tests := []struct {
		name   string
		amount int64
		ratios []int64
		want   []int64
	}{
		{"R$1,00 em 3 partes iguais", 100, []int64{1, 1, 1}, []int64{34, 33, 33}},
		{"70/30", 1000, []int64{70, 30}, []int64{700, 300}},
		{"sobra de 1 centavo", 101, []int64{1, 1}, []int64{51, 50}},
		{"valor negativo mantém o sinal", -100, []int64{1, 1, 1}, []int64{-34, -33, -33}},
		{"valor zero", 0, []int64{1, 2}, []int64{0, 0}},
		{"parte única recebe tudo", 999, []int64{5}, []int64{999}},
		{"MinInt64 não estoura", math.MinInt64, []int64{1}, []int64{math.MinInt64}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts, err := mustNew(t, tt.amount, money.BRL).Allocate(tt.ratios...)
			if err != nil {
				t.Fatal(err)
			}
			got := amounts(parts)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestAllocate_InvalidRatios(t *testing.T) {
	m := mustNew(t, 100, money.BRL)
	for _, ratios := range [][]int64{nil, {}, {0}, {1, -1}, {1, 0, 1}} {
		if _, err := m.Allocate(ratios...); !errors.Is(err, money.ErrInvalidAllocation) {
			t.Errorf("Allocate(%v) err = %v, want ErrInvalidAllocation", ratios, err)
		}
	}
}

// FuzzAllocate prova o invariante que importa: a soma das partes é SEMPRE o total.
// Rode com: go test -fuzz=FuzzAllocate -fuzztime=30s ./internal/domain/money
func FuzzAllocate(f *testing.F) {
	f.Add(int64(100), uint16(1), uint16(1), uint16(1))
	f.Add(int64(-1), uint16(3), uint16(7), uint16(1))
	f.Add(int64(math.MaxInt64), uint16(65535), uint16(1), uint16(2))
	f.Add(int64(math.MinInt64), uint16(1), uint16(1), uint16(1))

	f.Fuzz(func(t *testing.T, amount int64, r1, r2, r3 uint16) {
		m := mustNew(t, amount, money.BRL)
		ratios := []int64{int64(r1) + 1, int64(r2) + 1, int64(r3) + 1}

		parts, err := m.Allocate(ratios...)
		if err != nil {
			t.Fatalf("Allocate(%d, %v): %v", amount, ratios, err)
		}
		if len(parts) != len(ratios) {
			t.Fatalf("len = %d, want %d", len(parts), len(ratios))
		}

		// A soma é feita em big-free int64 com checagem: se somar sem estourar, tem que dar o total.
		sum := mustNew(t, 0, money.BRL)
		for _, p := range parts {
			if (amount >= 0 && p.IsNegative()) || (amount < 0 && p.IsPositive()) {
				t.Fatalf("parte com sinal trocado: %v de %d", p, amount)
			}
			sum, err = sum.Add(p)
			if err != nil {
				t.Fatalf("soma das partes estourou: %v", err)
			}
		}
		if sum.Amount() != amount {
			t.Fatalf("soma das partes = %d, want %d (partes %v)", sum.Amount(), amount, amounts(parts))
		}
	})
}
