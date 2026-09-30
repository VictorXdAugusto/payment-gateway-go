// Package outbox define o contrato do transactional outbox: eventos gravados junto com o
// estado do negócio e entregues depois, de forma assíncrona, ao webhook do lojista.
package outbox

import (
	"context"
	"errors"
	"time"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusDelivered Status = "delivered"
	StatusDead      Status = "dead"    // esgotou as tentativas: fica para inspeção e reenvio manual
	StatusSkipped   Status = "skipped" // lojista sem webhook configurado
)

// ErrClaimLost: a entrega já não é mais deste worker (o lease venceu e outro assumiu, ou
// ela já foi concluída). O resultado do worker antigo é descartado.
var ErrClaimLost = errors.New("entrega não pertence mais a este worker")

// Event é um fato de negócio a ser comunicado ao lojista.
type Event struct {
	EventID    string // único; o lojista usa para deduplicar (a entrega é at-least-once)
	MerchantID string
	Type       string
	PaymentID  string
	Payload    []byte // JSON já serializado: são estes bytes que vão assinados
	CreatedAt  time.Time
}

// Delivery é uma entrega reivindicada por um worker.
type Delivery struct {
	ID         int64 // id interno da linha
	Event      Event
	Attempt    int // número desta tentativa (1 na primeira); também é o "token" de fencing
	WebhookURL string
	Secret     string
}

type Repository interface {
	// Add grava eventos. Participa da transação do contexto: é o coração do padrão.
	Add(ctx context.Context, events ...Event) error

	// Claim reivindica até limit entregas vencidas e sem dono, marcando um lease.
	// Vários workers podem chamar ao mesmo tempo sem pegar a mesma linha (SKIP LOCKED).
	// Já incrementa o número de tentativas, então uma queda no meio conta como tentativa.
	Claim(ctx context.Context, limit int, lease time.Duration) ([]Delivery, error)

	MarkDelivered(ctx context.Context, d Delivery) error
	Reschedule(ctx context.Context, d Delivery, delay time.Duration, lastError string) error
	MarkDead(ctx context.Context, d Delivery, lastError string) error
	MarkSkipped(ctx context.Context, d Delivery, reason string) error
}

// ErrPermanent marca uma falha de entrega que repetir não resolve (ex.: URL proibida).
// O worker manda direto para dead em vez de gastar as tentativas.
var ErrPermanent = errors.New("falha permanente de entrega")
