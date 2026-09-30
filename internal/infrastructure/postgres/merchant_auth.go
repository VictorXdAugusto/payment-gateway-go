package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrUnauthorized = errors.New("api key inválida")

// MerchantAuthenticator troca uma API key pelo id do lojista.
// O banco guarda só o SHA-256 da key: um vazamento do banco não entrega credenciais.
type MerchantAuthenticator struct {
	pool *pgxpool.Pool
}

func NewMerchantAuthenticator(pool *pgxpool.Pool) *MerchantAuthenticator {
	return &MerchantAuthenticator{pool: pool}
}

// HashAPIKey é a função usada para guardar e para consultar a key.
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (a *MerchantAuthenticator) Authenticate(ctx context.Context, apiKey string) (string, error) {
	if apiKey == "" {
		return "", ErrUnauthorized
	}
	var id string
	err := a.pool.QueryRow(ctx,
		`SELECT id::text FROM merchants WHERE api_key_hash = $1`, HashAPIKey(apiKey)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUnauthorized
	}
	if err != nil {
		return "", fmt.Errorf("autenticar: %w", err)
	}
	return id, nil
}
