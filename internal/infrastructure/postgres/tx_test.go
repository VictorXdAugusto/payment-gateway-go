package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Um pânico dentro da transação não pode deixar a conexão presa: sem rollback, pânicos
// repetidos (o middleware os recupera e o processo segue vivo) esgotariam o pool.
func TestWithinTx_PanicRollsBackAndReleasesTheConnection(t *testing.T) {
	e := setup(t)
	// Prazo no Begin: sem rollback o pool esgota e o próximo Begin ficaria esperando para sempre;
	// com prazo o teste falha rápido e claro em vez de pendurar.
	ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
	defer cancel()
	for i := 0; i < 50; i++ { // bem mais que o tamanho do pool de teste (30)
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("o pânico devia continuar subindo (ou o Begin falhou: pool esgotado)")
				}
			}()
			_ = e.txm.WithinTx(ctx, func(ctx context.Context) error {
				if err := e.outbox().Add(ctx, e.event("evt_panic")); err != nil {
					t.Fatal(err)
				}
				panic("bug no repositório")
			})
		}()
	}

	if n := e.pool.Stat().AcquiredConns(); n != 0 {
		t.Errorf("%d conexões ainda presas depois dos pânicos", n)
	}
	var count int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM outbox_events`).Scan(&count); err != nil || count != 0 {
		t.Errorf("eventos = %d err=%v: o rollback devia ter desfeito tudo", count, err)
	}
}

func TestWithinTx_ErrorRollsBack_AndCommitKeepsTheWrites(t *testing.T) {
	e := setup(t)
	boom := errors.New("falha")
	err := e.txm.WithinTx(e.ctx, func(ctx context.Context) error {
		_ = e.outbox().Add(ctx, e.event("evt_1"))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if err := e.txm.WithinTx(e.ctx, func(ctx context.Context) error { return e.outbox().Add(ctx, e.event("evt_2")) }); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM outbox_events`).Scan(&count); err != nil || count != 1 {
		t.Errorf("eventos = %d err=%v, want 1 (só o commitado)", count, err)
	}
}
