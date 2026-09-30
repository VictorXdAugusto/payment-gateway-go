// Package pgtest cria um banco PostgreSQL descartável por teste, já com as migrations aplicadas.
//
// Os testes de integração leem TEST_DATABASE_URL (um banco onde o usuário possa CREATE
// DATABASE). Sem a variável, eles são PULADOS, então `go test ./...` continua rodando
// em qualquer máquina. Com ela, cada teste ganha um banco novo e isolado, e testes
// podem rodar em paralelo sem se enxergarem.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// New devolve um pool conectado a um banco novo. O banco é removido ao fim do teste.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()

	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_DATABASE_URL não definida: pulando teste de integração (rode `make test-integration`)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("conectar no banco de admin: %v", err)
	}
	defer admin.Close(ctx)

	name := "pgtest_" + randomHex(t)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("criar banco %s: %v", name, err)
	}

	cfg, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	cfg.MaxConns = 30 // os testes de concorrência abrem várias conexões

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("abrir pool em %s: %v", name, err)
	}
	applyMigrations(ctx, t, pool)

	t.Cleanup(func() {
		pool.Close()
		cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer ccancel()
		c, err := pgx.Connect(cctx, adminURL)
		if err != nil {
			t.Logf("cleanup: conectar: %v", err)
			return
		}
		defer c.Close(cctx)
		if _, err := c.Exec(cctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("cleanup: dropar %s: %v", name, err)
		}
	})
	return pool
}

func applyMigrations(ctx context.Context, t testing.TB, pool *pgxpool.Pool) {
	t.Helper()

	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..", "deployments", "migrations")

	files, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations não encontradas em %s (err=%v)", dir, err)
	}
	sort.Strings(files) // 000001, 000002... a ordem do nome é a ordem de aplicação

	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("aplicar %s: %v", filepath.Base(f), err)
		}
	}
}

func randomHex(t testing.TB) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// Merchant insere um lojista e devolve o id (UUID). Contas de lojista têm FK para merchants.
func Merchant(ctx context.Context, t testing.TB, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO merchants (name, api_key_hash) VALUES ($1, $2) RETURNING id::text`,
		name, fmt.Sprintf("hash-%s-%s", name, randomHex(t))).Scan(&id)
	if err != nil {
		t.Fatalf("inserir merchant: %v", err)
	}
	return id
}
