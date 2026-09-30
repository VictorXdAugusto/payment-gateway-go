package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DBTX é o que os repositórios precisam para falar com o banco. Tanto *pgxpool.Pool
// quanto pgx.Tx a satisfazem, então o repositório não sabe se está dentro de uma transação.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type txKey struct{}

// TxManager executa funções dentro de uma transação. A transação viaja no context:
// repositórios diferentes, chamados dentro do mesmo WithinTx, participam da MESMA
// transação sem que nenhum deles receba um *sql.Tx como parâmetro.
//
// É isso que vai permitir, nos próximos passos, gravar pagamento + ledger + outbox
// atomicamente: ou tudo entra, ou nada.
type TxManager struct {
	pool *pgxpool.Pool
}

func NewTxManager(pool *pgxpool.Pool) *TxManager { return &TxManager{pool: pool} }

// WithinTx roda fn numa transação: commit se fn devolver nil, rollback caso contrário.
// Se o ctx já carrega uma transação, fn apenas participa dela (quem abriu é quem comita).
func (m *TxManager) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return fn(ctx)
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		// Rollback com contexto próprio: se ctx foi cancelado, ainda precisamos desfazer.
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// DB devolve a transação do contexto, ou o pool se não houver nenhuma.
func (m *TxManager) DB(ctx context.Context) DBTX {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return tx
	}
	return m.pool
}
