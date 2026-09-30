// Comando worker entrega os eventos da outbox aos webhooks dos lojistas.
// Roda como processo separado do servidor HTTP: escala e falha de forma independente.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/config"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/webhook"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/worker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("worker encerrado com erro", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadWorker()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if cfg.AllowPrivate {
		log.Warn("WEBHOOK_ALLOW_PRIVATE=true: proteção contra SSRF DESLIGADA (só desenvolvimento)")
	}

	dispatcher := worker.NewDispatcher(
		postgres.NewOutboxRepository(postgres.NewTxManager(pool)),
		webhook.NewSender(webhook.Config{Timeout: cfg.DeliveryTimeout, AllowPrivate: cfg.AllowPrivate}),
		worker.Config{
			BatchSize: cfg.BatchSize, Concurrency: cfg.Concurrency, PollInterval: cfg.PollInterval,
			Lease: cfg.Lease, MaxAttempts: cfg.MaxAttempts,
			Backoff: worker.Backoff{Base: cfg.BaseBackoff, Max: cfg.MaxBackoff},
		},
		log,
	)

	log.Info("worker iniciado", "batch", cfg.BatchSize, "concurrency", cfg.Concurrency, "max_attempts", cfg.MaxAttempts)
	err = dispatcher.Run(ctx)
	log.Info("worker encerrado")
	return err
}
