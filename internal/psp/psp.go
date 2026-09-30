// Package psp define o contrato com o adquirente (Payment Service Provider) e o vocabulário
// de resultados. Toda chamada carrega uma chave de idempotência PRÓPRIA do PSP, para que
// repetir a chamada (retry, retomada após queda) nunca autorize ou capture duas vezes.
package psp

import (
	"context"
	"errors"
	"fmt"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
)

var (
	// ErrDeclined: o PSP respondeu que NÃO. Desfecho definitivo (ex.: saldo insuficiente).
	ErrDeclined = errors.New("recusado pelo PSP")

	// ErrIndeterminate: não sabemos o que aconteceu (timeout, 5xx, rede). A operação pode
	// ter acontecido ou não. NUNCA se assume "falhou": o estado fica unknown até descobrirmos.
	ErrIndeterminate = errors.New("resultado do PSP indeterminado")
)

// DeclinedError detalha uma recusa. errors.Is(err, ErrDeclined) continua valendo.
type DeclinedError struct {
	Code    string
	Message string
}

func (e *DeclinedError) Error() string {
	return fmt.Sprintf("%v: %s (%s)", ErrDeclined, e.Message, e.Code)
}
func (e *DeclinedError) Is(target error) bool { return target == ErrDeclined }

type AuthorizeRequest struct {
	IdempotencyKey string // chave do PSP, estável por pagamento
	Amount         money.Money
}

type Authorization struct {
	Reference string // id da autorização no PSP
}

type CaptureRequest struct {
	IdempotencyKey string
	Reference      string
	Amount         money.Money
}

type Outcome string

const (
	OutcomeAuthorized Outcome = "authorized"
	OutcomeDeclined   Outcome = "declined"
	OutcomeNotFound   Outcome = "not_found"
)

// LookupResult é o que o PSP sabe sobre uma chave de idempotência. Base da reconciliação.
type LookupResult struct {
	Outcome     Outcome
	Reference   string // preenchido se authorized
	DeclineCode string // preenchido se declined
}

type Gateway interface {
	// Authorize devolve *DeclinedError (recusa) ou ErrIndeterminate (não sabemos).
	Authorize(ctx context.Context, req AuthorizeRequest) (Authorization, error)

	// Capture efetiva uma autorização. Mesmos erros de Authorize.
	Capture(ctx context.Context, req CaptureRequest) error

	// Lookup pergunta ao PSP o que ele sabe sobre uma chave. Só lê, nunca causa efeito.
	Lookup(ctx context.Context, idempotencyKey string) (LookupResult, error)
}
