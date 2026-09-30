package usecase_test

import (
	"errors"
	"testing"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres/pgtest"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/usecase"
)

func TestBalance_IsDerivedFromTheLedger_AndFollowsRefunds(t *testing.T) {
	e := setup(t)
	uc := usecase.NewGetBalance(e.ledger)

	if got, err := uc.Execute(e.ctx, e.merchant); err != nil || len(got) != 0 {
		t.Fatalf("lojista sem movimento: %+v err=%v, want lista vazia", got, err)
	}

	id := e.captured(t, "a", 10000)
	if got, _ := uc.Execute(e.ctx, e.merchant); len(got) != 1 || got[0].Currency != "BRL" || got[0].Amount != 9710 {
		t.Fatalf("depois da captura: %+v, want BRL 9710", got)
	}
	if _, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-1", 10000)); err != nil {
		t.Fatal(err)
	}
	// Estornou tudo e a taxa não volta: o lojista fica devendo 290 (saldo negativo é legítimo).
	if got, _ := uc.Execute(e.ctx, e.merchant); len(got) != 1 || got[0].Amount != -290 {
		t.Fatalf("depois do estorno total: %+v, want -290", got)
	}
}

// O lojista só enxerga o que é dele.
func TestBalanceAndStatement_AreIsolatedPerMerchant(t *testing.T) {
	e := setup(t)
	e.captured(t, "a", 10000)
	other := pgtest.Merchant(e.ctx, t, e.pool, "outra-loja")

	if got, err := usecase.NewGetBalance(e.ledger).Execute(e.ctx, other); err != nil || len(got) != 0 {
		t.Errorf("saldo de outro lojista: %+v err=%v", got, err)
	}
	page, err := usecase.NewGetStatement(e.ledger).Execute(e.ctx, other, 0, 0)
	if err != nil || len(page.Data) != 0 || page.NextBefore != nil {
		t.Errorf("extrato de outro lojista: %+v err=%v", page, err)
	}
}

func TestStatement_ShowsNewestFirst_WithSignedAmounts(t *testing.T) {
	e := setup(t)
	id := e.captured(t, "a", 10000)
	if _, err := e.refund.Execute(e.ctx, e.refIn(id, "ref-1", 4000)); err != nil {
		t.Fatal(err)
	}

	page, err := usecase.NewGetStatement(e.ledger).Execute(e.ctx, e.merchant, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 2 || page.NextBefore != nil {
		t.Fatalf("página = %+v, want 2 linhas e sem próxima", page)
	}
	refund, capture := page.Data[0], page.Data[1]
	if refund.Type != "refund" || refund.Amount != -4000 || refund.PaymentID != id || refund.Currency != "BRL" {
		t.Errorf("estorno = %+v, want refund -4000", refund)
	}
	if capture.Type != "capture" || capture.Amount != 9710 { // líquido: a taxa não é do lojista
		t.Errorf("captura = %+v, want capture +9710", capture)
	}
	if refund.ID <= capture.ID {
		t.Errorf("ordem: %d deveria vir antes de %d (mais novo primeiro)", refund.ID, capture.ID)
	}
}

// Pagina sem repetir nem pular, e sabe quando acabou (inclusive na página exata).
func TestStatement_Pagination_WalksEverythingExactlyOnce(t *testing.T) {
	e := setup(t)
	for i := 0; i < 5; i++ {
		e.captured(t, string(rune('a'+i)), 1000)
	}
	uc := usecase.NewGetStatement(e.ledger)

	var seen []int64
	var before int64
	pages := 0
	for {
		page, err := uc.Execute(e.ctx, e.merchant, before, 2)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, l := range page.Data {
			seen = append(seen, l.ID)
		}
		if page.NextBefore == nil {
			break
		}
		before = *page.NextBefore
		if pages > 10 {
			t.Fatal("paginação não termina")
		}
	}
	if len(seen) != 5 || pages != 3 {
		t.Fatalf("linhas = %d em %d páginas, want 5 em 3", len(seen), pages)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] >= seen[i-1] {
			t.Fatalf("fora de ordem ou repetido: %v", seen)
		}
	}

	// Página que cabe exatamente: não anuncia uma próxima que não existe.
	page, _ := uc.Execute(e.ctx, e.merchant, 0, 5)
	if len(page.Data) != 5 || page.NextBefore != nil {
		t.Errorf("página exata: %d linhas, next=%v", len(page.Data), page.NextBefore)
	}
}

func TestStatement_RejectsInvalidParameters(t *testing.T) {
	e := setup(t)
	uc := usecase.NewGetStatement(e.ledger)
	for name, tc := range map[string]struct {
		before int64
		limit  int
	}{"limit negativo": {0, -1}, "limit acima do máximo": {0, usecase.MaxStatementLimit + 1}, "before negativo": {-1, 10}} {
		if _, err := uc.Execute(e.ctx, e.merchant, tc.before, tc.limit); !errors.Is(err, usecase.ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", name, err)
		}
	}
	if _, err := uc.Execute(e.ctx, e.merchant, 0, usecase.MaxStatementLimit); err != nil {
		t.Errorf("limit no máximo deve valer: %v", err)
	}
}
