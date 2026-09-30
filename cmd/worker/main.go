// Comando worker entrega os eventos da outbox aos webhooks dos lojistas.
// Roda como processo separado do servidor HTTP: escala e falha de forma independente.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/config"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres"
	pspclient "github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/psp"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/webhook"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/usecase"
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

	txm := postgres.NewTxManager(pool)
	outboxRepo := postgres.NewOutboxRepository(txm)
	payments := postgres.NewPaymentRepository(txm)
	keys := postgres.NewIdempotencyStore(txm, time.Minute) // o lease não é usado pela limpeza

	gateway := pspclient.New(pspclient.Config{
		BaseURL: cfg.PSPBaseURL, AttemptTimeout: cfg.PSPAttemptTimeout, MaxAttempts: cfg.PSPMaxAttempts,
		BaseBackoff: cfg.PSPBaseBackoff, MaxBackoff: cfg.PSPMaxBackoff,
	})
	newEventID := func() string { return "evt_" + strings.ReplaceAll(uuid.NewString(), "-", "") }
	resolve := usecase.NewResolveStuck(txm, payments, gateway, usecase.NewEventRecorder(outboxRepo, newEventID),
		cfg.UnknownGrace, time.Now)

	reconciler := worker.NewReconciler(payments, resolve, worker.ReconcilerConfig{
		Interval: cfg.ReconcileInterval, StaleAfter: cfg.ReconcileStaleAfter,
		PageSize: cfg.ReconcilePageSize, MaxConsecutiveErrors: cfg.ReconcileMaxErrors,
	}, log, time.Now)

	housekeeper := worker.NewHousekeeper([]worker.Retention{
		{Name: "idempotency_keys", OlderThan: cfg.IdempotencyRetention, Purger: keys},
		{Name: "outbox_events", OlderThan: cfg.OutboxRetention, Purger: outboxRepo},
	}, worker.HousekeeperConfig{Interval: cfg.RetentionInterval, Batch: cfg.RetentionBatch}, log)

	dispatcher := worker.NewDispatcher(
		outboxRepo,
		webhook.NewSender(webhook.Config{Timeout: cfg.DeliveryTimeout, AllowPrivate: cfg.AllowPrivate}),
		worker.Config{
			BatchSize: cfg.BatchSize, Concurrency: cfg.Concurrency, PollInterval: cfg.PollInterval,
			Lease: cfg.Lease, MaxAttempts: cfg.MaxAttempts,
			Backoff: worker.Backoff{Base: cfg.BaseBackoff, Max: cfg.MaxBackoff},
		},
		log,
	)

	log.Info("worker iniciado", "batch", cfg.BatchSize, "concurrency", cfg.Concurrency, "max_attempts", cfg.MaxAttempts,
		"reconcile_interval", cfg.ReconcileInterval.String(), "unknown_grace", cfg.UnknownGrace.String())

	// Três laços independentes no mesmo processo. Todos param quando ctx é cancelado, e o
	// processo só sai depois de os três terminarem (as entregas em voo são concluídas).
	var wg sync.WaitGroup
	for _, loop := range []func(){func() { reconciler.Run(ctx) }, func() { housekeeper.Run(ctx) }} {
		wg.Add(1)
		go func() { defer wg.Done(); loop() }()
	}
	err = dispatcher.Run(ctx)
	wg.Wait()
	log.Info("worker encerrado")
	return err
}
