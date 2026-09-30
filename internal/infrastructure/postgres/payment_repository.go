package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
)

const (
	pgUniqueViolation     = "23505"
	paymentIdemConstraint = "payments_merchant_idempotency_key"
)

type PaymentRepository struct {
	tx *TxManager
}

var _ payment.Repository = (*PaymentRepository)(nil)

func NewPaymentRepository(tx *TxManager) *PaymentRepository { return &PaymentRepository{tx: tx} }

func (r *PaymentRepository) Insert(ctx context.Context, p *payment.Payment, idempotencyKey string) error {
	s := p.Snapshot()
	_, err := r.tx.DB(ctx).Exec(ctx, `
		INSERT INTO payments (id, merchant_id, idempotency_key, amount, currency, refunded_amount,
		                      status, psp_reference, failure_reason, version, created_at, updated_at)
		VALUES ($1, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		string(s.ID), string(s.MerchantID), idempotencyKey, s.Amount.Amount(), string(s.Amount.Currency()),
		s.Refunded.Amount(), string(s.Status), s.PSPReference, s.FailureReason, s.Version, s.CreatedAt, s.UpdatedAt)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == paymentIdemConstraint {
		return payment.ErrDuplicate
	}
	if err != nil {
		return fmt.Errorf("inserir pagamento: %w", err)
	}
	return nil
}

const selectPayment = `
	SELECT id, merchant_id::text, amount, currency, refunded_amount, status,
	       psp_reference, failure_reason, version, created_at, updated_at
	  FROM payments`

func (r *PaymentRepository) Get(ctx context.Context, merchantID payment.MerchantID, id payment.ID) (*payment.Payment, error) {
	return r.scan(r.tx.DB(ctx).QueryRow(ctx, selectPayment+` WHERE merchant_id = $1::uuid AND id = $2`,
		string(merchantID), string(id)), id)
}

func (r *PaymentRepository) GetByID(ctx context.Context, id payment.ID) (*payment.Payment, error) {
	return r.scan(r.tx.DB(ctx).QueryRow(ctx, selectPayment+` WHERE id = $1`, string(id)), id)
}

func (r *PaymentRepository) scan(row pgx.Row, id payment.ID) (*payment.Payment, error) {
	var (
		s                     payment.Snapshot
		amount, refunded      int64
		currency, status      string
		merchant, pspRef, why string
	)
	err := row.Scan(&s.ID, &merchant, &amount, &currency, &refunded, &status,
		&pspRef, &why, &s.Version, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, payment.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("buscar pagamento %s: %w", id, err)
	}

	cur := money.Currency(currency)
	if s.Amount, err = money.New(amount, cur); err != nil {
		return nil, err
	}
	if s.Refunded, err = money.New(refunded, cur); err != nil {
		return nil, err
	}
	s.MerchantID = payment.MerchantID(merchant)
	s.Status = payment.Status(status)
	s.PSPReference, s.FailureReason = pspRef, why

	// Restore valida os invariantes: dado corrompido no banco falha aqui, alto e claro.
	return payment.Restore(s)
}

func (r *PaymentRepository) Update(ctx context.Context, p *payment.Payment) error {
	s := p.Snapshot()
	db := r.tx.DB(ctx)

	tag, err := db.Exec(ctx, `
		UPDATE payments
		   SET refunded_amount = $1, status = $2, psp_reference = $3, failure_reason = $4,
		       updated_at = $5, version = version + 1
		 WHERE id = $6 AND merchant_id = $7::uuid AND version = $8`,
		s.Refunded.Amount(), string(s.Status), s.PSPReference, s.FailureReason,
		s.UpdatedAt, string(s.ID), string(s.MerchantID), s.Version)
	if err != nil {
		return fmt.Errorf("atualizar pagamento %s: %w", s.ID, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	// Nenhuma linha: ou o pagamento não existe, ou a versão andou.
	var exists bool
	if err := db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM payments WHERE id = $1 AND merchant_id = $2::uuid)`,
		string(s.ID), string(s.MerchantID)).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return payment.ErrNotFound
	}
	return payment.ErrConcurrentModification
}
