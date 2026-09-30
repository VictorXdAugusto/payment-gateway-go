package postgres_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/idempotency"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres/pgtest"
)

const testLease = 30 * time.Second

func (e *env) keys() *postgres.IdempotencyStore {
	return postgres.NewIdempotencyStore(e.txm, testLease)
}

func (e *env) key(v string) idempotency.Key {
	return idempotency.Key{MerchantID: e.merchant, Value: v}
}

// expireLease simula "o dono da chave morreu há 1h": empurra o lease para o passado.
func (e *env) expireLease(t testing.TB, k idempotency.Key) {
	t.Helper()
	if _, err := e.pool.Exec(e.ctx,
		`UPDATE idempotency_keys SET locked_at = now() - interval '1 hour' WHERE merchant_id = $1::uuid AND key = $2`,
		k.MerchantID, k.Value); err != nil {
		t.Fatal(err)
	}
}

func TestIdempotencyStore_FirstAcquire(t *testing.T) {
	e := setup(t)
	a, err := e.keys().Acquire(e.ctx, e.key("k1"), "hash-a")
	if err != nil {
		t.Fatal(err)
	}
	if a.Token != 1 || a.RecoveryPoint != idempotency.PointStarted || a.Replay != nil || a.ResourceID != "" {
		t.Errorf("aquisição inicial errada: %+v", a)
	}
}

func TestIdempotencyStore_InFlight_RejectsSecondCaller(t *testing.T) {
	e := setup(t)
	if _, err := e.keys().Acquire(e.ctx, e.key("k1"), "hash-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.keys().Acquire(e.ctx, e.key("k1"), "hash-a"); !errors.Is(err, idempotency.ErrInFlight) {
		t.Fatalf("err = %v, want ErrInFlight", err)
	}
}

func TestIdempotencyStore_HashMismatch(t *testing.T) {
	e := setup(t)
	a, _ := e.keys().Acquire(e.ctx, e.key("k1"), "hash-a")

	// Em andamento E depois de terminada: mesma chave + corpo diferente é sempre erro de uso.
	if _, err := e.keys().Acquire(e.ctx, e.key("k1"), "hash-B"); !errors.Is(err, idempotency.ErrKeyMismatch) {
		t.Fatalf("em andamento: err = %v, want ErrKeyMismatch", err)
	}
	if err := e.keys().Finish(e.ctx, a, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.keys().Acquire(e.ctx, e.key("k1"), "hash-B"); !errors.Is(err, idempotency.ErrKeyMismatch) {
		t.Fatalf("terminada: err = %v, want ErrKeyMismatch", err)
	}
}

func TestIdempotencyStore_Finish_ThenReplay(t *testing.T) {
	e := setup(t)
	a, _ := e.keys().Acquire(e.ctx, e.key("k1"), "hash-a")
	if err := e.keys().Advance(e.ctx, a, "step_2", "res_1"); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"id":"res_1","valor":  10}`) // bytes exatos, inclusive espaços, voltam iguais
	if err := e.keys().Finish(e.ctx, a, body); err != nil {
		t.Fatal(err)
	}

	got, err := e.keys().Acquire(e.ctx, e.key("k1"), "hash-a")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Replay) != string(body) || got.RecoveryPoint != idempotency.PointFinished {
		t.Errorf("replay = %q (%s), want %q", got.Replay, got.RecoveryPoint, body)
	}
}

func TestIdempotencyStore_Release_AllowsResumeFromRecoveryPoint(t *testing.T) {
	e := setup(t)
	a, _ := e.keys().Acquire(e.ctx, e.key("k1"), "hash-a")
	if err := e.keys().Advance(e.ctx, a, "payment_created", "pay_9"); err != nil {
		t.Fatal(err)
	}
	if err := e.keys().Release(e.ctx, a); err != nil {
		t.Fatal(err)
	}

	b, err := e.keys().Acquire(e.ctx, e.key("k1"), "hash-a")
	if err != nil {
		t.Fatalf("depois do Release o retry deveria assumir na hora: %v", err)
	}
	if b.RecoveryPoint != "payment_created" || b.ResourceID != "pay_9" || b.Token != 2 {
		t.Errorf("retomada errada: %+v", b)
	}
}

// O cenário de "o processo morreu": ninguém chamou Release. Depois do lease outro assume,
// e o dono antigo (que pode ter só travado por um GC longo) perde o direito de escrever.
func TestIdempotencyStore_LeaseExpiry_TakeoverAndFencing(t *testing.T) {
	e := setup(t)
	old, _ := e.keys().Acquire(e.ctx, e.key("k1"), "hash-a")

	if _, err := e.keys().Acquire(e.ctx, e.key("k1"), "hash-a"); !errors.Is(err, idempotency.ErrInFlight) {
		t.Fatalf("dentro do lease: err = %v, want ErrInFlight", err)
	}

	e.expireLease(t, e.key("k1"))
	fresh, err := e.keys().Acquire(e.ctx, e.key("k1"), "hash-a")
	if err != nil {
		t.Fatalf("depois do lease deveria assumir: %v", err)
	}
	if fresh.Token != 2 {
		t.Errorf("token = %d, want 2", fresh.Token)
	}

	// Fencing: o dono antigo acorda e tenta escrever com o token 1.
	if err := e.keys().Advance(e.ctx, old, "payment_created", "pay_old"); !errors.Is(err, idempotency.ErrLockLost) {
		t.Errorf("Advance do dono antigo: err = %v, want ErrLockLost", err)
	}
	if err := e.keys().Finish(e.ctx, old, []byte(`{"velho":true}`)); !errors.Is(err, idempotency.ErrLockLost) {
		t.Errorf("Finish do dono antigo: err = %v, want ErrLockLost", err)
	}
	if err := e.keys().Release(e.ctx, old); err != nil {
		t.Errorf("Release do dono antigo não deve falhar: %v", err)
	}
	// ...e o Release do dono antigo NÃO pode soltar o lock do novo.
	if _, err := e.keys().Acquire(e.ctx, e.key("k1"), "hash-a"); !errors.Is(err, idempotency.ErrInFlight) {
		t.Errorf("o Release velho soltou o lock do dono novo: err = %v", err)
	}

	if err := e.keys().Finish(e.ctx, fresh, []byte(`{"novo":true}`)); err != nil {
		t.Errorf("o dono atual deveria conseguir terminar: %v", err)
	}
}

func TestIdempotencyStore_KeysAreIsolatedPerMerchant(t *testing.T) {
	e := setup(t)
	other := pgtest.Merchant(e.ctx, t, e.pool, "outra")

	if _, err := e.keys().Acquire(e.ctx, e.key("same"), "h"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.keys().Acquire(e.ctx, idempotency.Key{MerchantID: other, Value: "same"}, "h"); err != nil {
		t.Fatalf("mesma chave em outro lojista não pode colidir: %v", err)
	}
}

// Advance dentro de uma transação que faz rollback não pode deixar rastro: é isso que
// amarra "efeito da fase" e "avanço do recovery point" numa coisa só.
func TestIdempotencyStore_Advance_IsAtomicWithTransaction(t *testing.T) {
	e := setup(t)
	a, _ := e.keys().Acquire(e.ctx, e.key("k1"), "hash-a")
	boom := errors.New("falha depois do Advance")

	err := e.txm.WithinTx(e.ctx, func(ctx context.Context) error {
		if err := e.keys().Advance(ctx, a, "payment_created", "pay_1"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}

	if err := e.keys().Release(e.ctx, a); err != nil {
		t.Fatal(err)
	}
	b, _ := e.keys().Acquire(e.ctx, e.key("k1"), "hash-a")
	if b.RecoveryPoint != idempotency.PointStarted || b.ResourceID != "" {
		t.Errorf("o Advance sobreviveu ao rollback: %+v", b)
	}
}

// 30 requisições simultâneas com a mesma chave: UMA adquire, 29 ficam sabendo que está em andamento.
func TestIdempotencyStore_ConcurrentAcquire_ExactlyOneWins(t *testing.T) {
	e := setup(t)
	const callers = 30

	var wg sync.WaitGroup
	var won, inFlight atomic.Int64
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			switch _, err := e.keys().Acquire(e.ctx, e.key("race"), "hash-a"); {
			case err == nil:
				won.Add(1)
			case errors.Is(err, idempotency.ErrInFlight):
				inFlight.Add(1)
			default:
				t.Errorf("erro inesperado: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if won.Load() != 1 || inFlight.Load() != callers-1 {
		t.Fatalf("won=%d inFlight=%d, want 1 e %d", won.Load(), inFlight.Load(), callers-1)
	}
}

// Depois do lease expirado, 20 retries simultâneos disputam a chave abandonada: UM assume.
func TestIdempotencyStore_ConcurrentTakeover_ExactlyOneWins(t *testing.T) {
	e := setup(t)
	if _, err := e.keys().Acquire(e.ctx, e.key("dead"), "hash-a"); err != nil {
		t.Fatal(err)
	}
	e.expireLease(t, e.key("dead"))

	var wg sync.WaitGroup
	var won atomic.Int64
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := e.keys().Acquire(e.ctx, e.key("dead"), "hash-a"); err == nil {
				won.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if won.Load() != 1 {
		t.Fatalf("%d processos assumiram a mesma chave abandonada, want 1", won.Load())
	}
}

func TestIdempotencyStore_Purge_OnlyRemovesOldFinishedKeys(t *testing.T) {
	e := setup(t)
	store := e.keys()

	finish := func(v string) {
		a, err := store.Acquire(e.ctx, e.key(v), "h")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Finish(e.ctx, a, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	finish("old-finished")
	finish("recent-finished")
	if _, err := store.Acquire(e.ctx, e.key("old-inflight"), "h"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE idempotency_keys SET created_at = now() - interval '48 hours'
		WHERE key IN ('old-finished', 'old-inflight')`); err != nil {
		t.Fatal(err)
	}

	n, err := store.Purge(e.ctx, 24*time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("apagou %d, want 1", n)
	}
	if e.count(t, "idempotency_keys") != 2 {
		t.Error("só a chave antiga e TERMINADA pode ser apagada")
	}
}

// Lacuna achada por mutação: com o lease expirado, um retry com corpo DIFERENTE não pode
// assumir a chave abandonada (retomaria a operação de outro corpo com dados errados).
func TestIdempotencyStore_ExpiredLease_DoesNotLetADifferentBodyTakeOver(t *testing.T) {
	e := setup(t)
	if _, err := e.keys().Acquire(e.ctx, e.key("k1"), "hash-a"); err != nil {
		t.Fatal(err)
	}
	e.expireLease(t, e.key("k1"))

	if _, err := e.keys().Acquire(e.ctx, e.key("k1"), "hash-B"); !errors.Is(err, idempotency.ErrKeyMismatch) {
		t.Fatalf("err = %v, want ErrKeyMismatch", err)
	}

	// O dono legítimo (mesmo corpo) ainda consegue assumir, com o token seguinte.
	a, err := e.keys().Acquire(e.ctx, e.key("k1"), "hash-a")
	if err != nil || a.Token != 2 {
		t.Fatalf("retry legítimo: %+v / %v (o mismatch não pode ter consumido o token)", a, err)
	}
}
