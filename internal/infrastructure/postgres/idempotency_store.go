package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/idempotency"
)

// IdempotencyStore implementa idempotency.Store sobre PostgreSQL.
//
// O relógio é o do BANCO (now()), nunca o da aplicação: com várias instâncias, o lease
// não pode depender de relógios de máquinas diferentes estarem sincronizados.
type IdempotencyStore struct {
	tx    *TxManager
	lease time.Duration
}

var _ idempotency.Store = (*IdempotencyStore)(nil)

// NewIdempotencyStore cria o store. lease é quanto tempo uma requisição "segura" a chave
// antes de outra poder assumir (deve ser maior que o pior caso de processamento).
func NewIdempotencyStore(tx *TxManager, lease time.Duration) *IdempotencyStore {
	return &IdempotencyStore{tx: tx, lease: lease}
}

func (s *IdempotencyStore) Acquire(ctx context.Context, key idempotency.Key, requestHash string) (idempotency.Acquisition, error) {
	db := s.tx.DB(ctx)

	// Três tentativas cobrem a corrida rara em que a linha é apagada (purge) entre passos.
	for attempt := 0; attempt < 3; attempt++ {
		// 1) Primeira vez: só UMA requisição concorrente vence este INSERT.
		tag, err := db.Exec(ctx, `
			INSERT INTO idempotency_keys (merchant_id, key, request_hash, recovery_point, locked_at)
			VALUES ($1::uuid, $2, $3, $4, now())
			ON CONFLICT (merchant_id, key) DO NOTHING`,
			key.MerchantID, key.Value, requestHash, idempotency.PointStarted)
		if err != nil {
			return idempotency.Acquisition{}, fmt.Errorf("registrar chave: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return idempotency.Acquisition{Key: key, Token: 1, RecoveryPoint: idempotency.PointStarted}, nil
		}

		// 2) Já existia: assume SE não terminou, o corpo é o mesmo e ninguém está com o lease.
		//    Um único UPDATE atômico decide, então dois retries nunca assumem juntos.
		a := idempotency.Acquisition{Key: key}
		err = db.QueryRow(ctx, `
			UPDATE idempotency_keys
			   SET locked_at = now(), lock_token = lock_token + 1, updated_at = now()
			 WHERE merchant_id = $1::uuid AND key = $2
			   AND request_hash = $3
			   AND recovery_point <> $4
			   AND (locked_at IS NULL OR locked_at < now() - make_interval(secs => $5::float8))
			RETURNING recovery_point, resource_id, lock_token`,
			key.MerchantID, key.Value, requestHash, idempotency.PointFinished, s.lease.Seconds()).
			Scan(&a.RecoveryPoint, &a.ResourceID, &a.Token)
		if err == nil {
			return a, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return idempotency.Acquisition{}, fmt.Errorf("assumir chave: %w", err)
		}

		// 3) Não deu para assumir: descobre por quê.
		var (
			storedHash string
			point      string
			response   []byte
		)
		err = db.QueryRow(ctx, `
			SELECT request_hash, recovery_point, response_body
			  FROM idempotency_keys WHERE merchant_id = $1::uuid AND key = $2`,
			key.MerchantID, key.Value).Scan(&storedHash, &point, &response)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // sumiu no meio; tenta de novo
		}
		if err != nil {
			return idempotency.Acquisition{}, fmt.Errorf("ler chave: %w", err)
		}

		switch {
		case storedHash != requestHash:
			return idempotency.Acquisition{}, idempotency.ErrKeyMismatch
		case point == idempotency.PointFinished:
			a.RecoveryPoint, a.Replay = point, response
			return a, nil
		default:
			return idempotency.Acquisition{}, idempotency.ErrInFlight
		}
	}
	return idempotency.Acquisition{}, errors.New("não foi possível adquirir a chave de idempotência")
}

func (s *IdempotencyStore) Advance(ctx context.Context, a idempotency.Acquisition, point, resourceID string) error {
	tag, err := s.tx.DB(ctx).Exec(ctx, `
		UPDATE idempotency_keys
		   SET recovery_point = $3,
		       resource_id    = CASE WHEN $4 = '' THEN resource_id ELSE $4 END,
		       updated_at     = now()
		 WHERE merchant_id = $1::uuid AND key = $2
		   AND lock_token = $5 AND recovery_point <> $6`,
		a.Key.MerchantID, a.Key.Value, point, resourceID, a.Token, idempotency.PointFinished)
	if err != nil {
		return fmt.Errorf("avançar chave: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return idempotency.ErrLockLost
	}
	return nil
}

func (s *IdempotencyStore) Finish(ctx context.Context, a idempotency.Acquisition, response []byte) error {
	tag, err := s.tx.DB(ctx).Exec(ctx, `
		UPDATE idempotency_keys
		   SET recovery_point = $3, response_body = $4, locked_at = NULL, updated_at = now()
		 WHERE merchant_id = $1::uuid AND key = $2
		   AND lock_token = $5 AND recovery_point <> $3`,
		a.Key.MerchantID, a.Key.Value, idempotency.PointFinished, response, a.Token)
	if err != nil {
		return fmt.Errorf("finalizar chave: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return idempotency.ErrLockLost
	}
	return nil
}

func (s *IdempotencyStore) Release(ctx context.Context, a idempotency.Acquisition) error {
	_, err := s.tx.DB(ctx).Exec(ctx, `
		UPDATE idempotency_keys SET locked_at = NULL, updated_at = now()
		 WHERE merchant_id = $1::uuid AND key = $2
		   AND lock_token = $3 AND recovery_point <> $4`,
		a.Key.MerchantID, a.Key.Value, a.Token, idempotency.PointFinished)
	if err != nil {
		return fmt.Errorf("liberar chave: %w", err)
	}
	return nil
}

// PurgeOlderThan apaga chaves antigas (a Stripe guarda 24h). Só apaga chaves TERMINADAS:
// uma em andamento nunca é removida por baixo de quem a processa.
func (s *IdempotencyStore) PurgeOlderThan(ctx context.Context, age time.Duration) (int64, error) {
	tag, err := s.tx.DB(ctx).Exec(ctx, `
		DELETE FROM idempotency_keys
		 WHERE recovery_point = $1 AND created_at < now() - make_interval(secs => $2::float8)`,
		idempotency.PointFinished, age.Seconds())
	if err != nil {
		return 0, fmt.Errorf("limpar chaves: %w", err)
	}
	return tag.RowsAffected(), nil
}
