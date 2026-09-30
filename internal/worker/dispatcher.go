// Package worker entrega os eventos da outbox aos webhooks dos lojistas.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/outbox"
)

// Deliverer faz UMA tentativa de entrega. nil = entregue.
type Deliverer interface {
	Deliver(ctx context.Context, d outbox.Delivery) error
}

type Config struct {
	BatchSize    int           // entregas reivindicadas por rodada
	Concurrency  int           // entregas simultâneas
	PollInterval time.Duration // espera quando não há nada a fazer
	Lease        time.Duration // por quanto tempo uma entrega fica "reservada" a este worker
	MaxAttempts  int           // depois disso, dead letter
	Backoff      Backoff
}

type Dispatcher struct {
	repo   outbox.Repository
	sender Deliverer
	cfg    Config
	log    *slog.Logger
}

func NewDispatcher(repo outbox.Repository, sender Deliverer, cfg Config, log *slog.Logger) *Dispatcher {
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.BatchSize < 1 {
		cfg.BatchSize = 1
	}
	return &Dispatcher{repo: repo, sender: sender, cfg: cfg, log: log}
}

// Run processa até ctx ser cancelado. No cancelamento NÃO abandona entregas em andamento:
// termina as que já começaram (o resultado delas precisa ser gravado) e só então retorna.
func (d *Dispatcher) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		n, err := d.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			d.log.Error("rodada da outbox falhou", "error", err)
		}
		// Rodada cheia: provavelmente há mais coisa esperando, não dorme.
		if err == nil && n >= d.cfg.BatchSize {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(d.cfg.PollInterval):
		}
	}
	return nil
}

// RunOnce reivindica um lote, entrega com concorrência limitada e espera todas terminarem.
// Devolve quantas entregas processou.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	batch, err := d.repo.Claim(ctx, d.cfg.BatchSize, d.cfg.Lease)
	if err != nil {
		return 0, err
	}

	// Depois de reivindicar, a entrega é nossa: um cancelamento de ctx não pode abortar o HTTP
	// no meio e deixar o resultado sem gravar. O timeout do próprio sender limita a espera.
	work := context.WithoutCancel(ctx)

	sem := make(chan struct{}, d.cfg.Concurrency)
	var wg sync.WaitGroup
	for _, delivery := range batch {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			d.process(work, delivery)
		}()
	}
	wg.Wait()
	return len(batch), nil
}

func (d *Dispatcher) process(ctx context.Context, del outbox.Delivery) {
	log := d.log.With("event_id", del.Event.EventID, "type", del.Event.Type, "attempt", del.Attempt)

	if del.WebhookURL == "" {
		d.record(log, d.repo.MarkSkipped(ctx, del, "lojista sem webhook configurado"), "skipped")
		return
	}

	err := d.sender.Deliver(ctx, del)
	switch {
	case err == nil:
		d.record(log, d.repo.MarkDelivered(ctx, del), "delivered")

	case errors.Is(err, outbox.ErrPermanent):
		log.Warn("falha permanente: indo direto para dead letter", "error", err)
		d.record(log, d.repo.MarkDead(ctx, del, err.Error()), "dead")

	case del.Attempt >= d.cfg.MaxAttempts:
		log.Warn("tentativas esgotadas: dead letter", "error", err)
		d.record(log, d.repo.MarkDead(ctx, del, err.Error()), "dead")

	default:
		delay := d.cfg.Backoff.Delay(del.Attempt)
		log.Info("entrega falhou, reagendando", "error", err, "retry_in", delay.String())
		d.record(log, d.repo.Reschedule(ctx, del, delay, err.Error()), "rescheduled")
	}
}

func (d *Dispatcher) record(log *slog.Logger, err error, outcome string) {
	switch {
	case err == nil:
		log.Info("entrega processada", "outcome", outcome)
	case errors.Is(err, outbox.ErrClaimLost):
		// O lease venceu enquanto entregávamos e outro worker assumiu. O resultado dele vale.
		log.Info("resultado descartado: entrega reassumida por outro worker")
	default:
		// Não gravamos o resultado: o lease vai expirar e a entrega será refeita (at-least-once).
		log.Error("falha ao gravar o resultado da entrega", "error", err)
	}
}
