package payment

import (
	"context"
	"errors"
)

var (
	ErrNotFound               = errors.New("pagamento não encontrado")
	ErrConcurrentModification = errors.New("pagamento foi alterado por outra operação")
	ErrDuplicate              = errors.New("já existe pagamento para esta chave de idempotência")
)

// Repository é a porta de persistência do agregado Payment.
type Repository interface {
	// Insert grava um pagamento novo. idempotencyKey vincula o pagamento à chave que o criou.
	Insert(ctx context.Context, p *Payment, idempotencyKey string) error

	// Get busca o pagamento de UM lojista. Id de outro lojista é ErrNotFound, nunca vaza.
	Get(ctx context.Context, merchantID MerchantID, id ID) (*Payment, error)

	// GetByID busca sem escopo de lojista. Uso INTERNO (reconciliação); a API nunca deve usá-lo.
	GetByID(ctx context.Context, id ID) (*Payment, error)

	// Update grava as mudanças com lock otimista: só vale se a versão no banco ainda for a
	// que o agregado tinha quando foi carregado. Senão, ErrConcurrentModification.
	// Depois de um Update bem-sucedido, recarregue o agregado antes de mudá-lo de novo.
	Update(ctx context.Context, p *Payment) error
}
