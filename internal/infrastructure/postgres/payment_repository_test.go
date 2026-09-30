package postgres_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres/pgtest"
)

func (e *env) payments() *postgres.PaymentRepository { return postgres.NewPaymentRepository(e.txm) }

func (e *env) newPayment(t testing.TB, id string, cents int64) *payment.Payment {
	t.Helper()
	p, err := payment.New(payment.ID(id), payment.MerchantID(e.merchant), brl(t, cents), now)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPaymentRepository_InsertAndGet_RoundTrip(t *testing.T) {
	e := setup(t)
	p := e.newPayment(t, "pay_1", 12345)

	if err := e.payments().Insert(e.ctx, p, "key-1"); err != nil {
		t.Fatal(err)
	}
	got, err := e.payments().Get(e.ctx, payment.MerchantID(e.merchant), "pay_1")
	if err != nil {
		t.Fatal(err)
	}

	if got.ID() != "pay_1" || got.Amount().Amount() != 12345 || got.Amount().Currency() != money.BRL ||
		got.Status() != payment.StatusCreated || got.Version() != 0 || !got.RefundedAmount().IsZero() {
		t.Errorf("round trip errado: %+v", got.Snapshot())
	}
	if !got.CreatedAt().Equal(now) {
		t.Errorf("created_at = %v, want %v", got.CreatedAt(), now)
	}
}

func TestPaymentRepository_Get_IsolatedPerMerchant(t *testing.T) {
	e := setup(t)
	if err := e.payments().Insert(e.ctx, e.newPayment(t, "pay_1", 100), "k"); err != nil {
		t.Fatal(err)
	}
	other := pgtest.Merchant(e.ctx, t, e.pool, "outra-loja")

	if _, err := e.payments().Get(e.ctx, payment.MerchantID(other), "pay_1"); !errors.Is(err, payment.ErrNotFound) {
		t.Fatalf("pagamento de outro lojista vazou: err = %v", err)
	}
	if _, err := e.payments().Get(e.ctx, payment.MerchantID(e.merchant), "nao_existe"); !errors.Is(err, payment.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestPaymentRepository_Insert_RejectsDuplicateIdempotencyKey(t *testing.T) {
	e := setup(t)
	if err := e.payments().Insert(e.ctx, e.newPayment(t, "pay_1", 100), "same-key"); err != nil {
		t.Fatal(err)
	}
	err := e.payments().Insert(e.ctx, e.newPayment(t, "pay_2", 100), "same-key")
	if !errors.Is(err, payment.ErrDuplicate) {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}

	// A mesma chave em outro lojista é permitida: as chaves são isoladas por lojista.
	other := pgtest.Merchant(e.ctx, t, e.pool, "outra-loja")
	p, _ := payment.New("pay_3", payment.MerchantID(other), brl(t, 100), now)
	if err := e.payments().Insert(e.ctx, p, "same-key"); err != nil {
		t.Fatalf("mesma chave em outro lojista deveria funcionar: %v", err)
	}
}

func TestPaymentRepository_Update_OptimisticLock(t *testing.T) {
	e := setup(t)
	repo := e.payments()
	if err := repo.Insert(e.ctx, e.newPayment(t, "pay_1", 10000), "k"); err != nil {
		t.Fatal(err)
	}
	mid := payment.MerchantID(e.merchant)

	// Duas "requisições" carregam a MESMA versão.
	a, _ := repo.Get(e.ctx, mid, "pay_1")
	b, _ := repo.Get(e.ctx, mid, "pay_1")

	if err := a.Authorize("psp_1", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Update(e.ctx, a); err != nil {
		t.Fatalf("primeira escrita deveria vencer: %v", err)
	}

	if err := b.Fail("negado", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Update(e.ctx, b); !errors.Is(err, payment.ErrConcurrentModification) {
		t.Fatalf("escrita com versão velha: err = %v, want ErrConcurrentModification", err)
	}

	got, _ := repo.Get(e.ctx, mid, "pay_1")
	if got.Status() != payment.StatusAuthorized || got.Version() != 1 || got.PSPReference() != "psp_1" {
		t.Errorf("a escrita perdedora não pode ter deixado marca: %+v", got.Snapshot())
	}
}

func TestPaymentRepository_Update_NotFound(t *testing.T) {
	e := setup(t)
	ghost := e.newPayment(t, "fantasma", 100)
	if err := e.payments().Update(e.ctx, ghost); !errors.Is(err, payment.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// 20 estornos de R$ 10,00 disputando um pagamento de R$ 100,00: o lock otimista tem que
// deixar passar exatamente 10 e o total estornado nunca ultrapassar o capturado.
func TestPaymentRepository_ConcurrentRefunds_NeverExceedCaptured(t *testing.T) {
	e := setup(t)
	repo := e.payments()
	mid := payment.MerchantID(e.merchant)

	p := e.newPayment(t, "pay_1", 10000)
	if err := repo.Insert(e.ctx, p, "k"); err != nil {
		t.Fatal(err)
	}
	loaded, _ := repo.Get(e.ctx, mid, "pay_1")
	if err := loaded.Authorize("psp_1", now); err != nil {
		t.Fatal(err)
	}
	if err := loaded.Capture(now); err != nil {
		t.Fatal(err)
	}
	if err := repo.Update(e.ctx, loaded); err != nil {
		t.Fatal(err)
	}

	var applied, rejected, retries atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				cur, err := repo.Get(e.ctx, mid, "pay_1")
				if err != nil {
					t.Errorf("get: %v", err)
					return
				}
				if err := cur.Refund(brl(t, 1000), now); err != nil {
					rejected.Add(1) // domínio recusou: já estornou tudo
					return
				}
				switch err := repo.Update(e.ctx, cur); {
				case err == nil:
					applied.Add(1)
					return
				case errors.Is(err, payment.ErrConcurrentModification):
					retries.Add(1) // perdeu a corrida: recarrega e tenta de novo
				default:
					t.Errorf("update: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	final, _ := repo.Get(e.ctx, mid, "pay_1")
	if applied.Load() != 10 || rejected.Load() != 10 {
		t.Errorf("aplicados=%d rejeitados=%d, want 10 e 10", applied.Load(), rejected.Load())
	}
	if final.RefundedAmount().Amount() != 10000 || final.Status() != payment.StatusRefunded {
		t.Errorf("final: estornado=%s status=%s", final.RefundedAmount(), final.Status())
	}
	if final.Version() != 11 { // 1 (authorize+capture) + 10 estornos
		t.Errorf("version = %d, want 11", final.Version())
	}
	t.Logf("conflitos resolvidos por retry: %d", retries.Load())
}
