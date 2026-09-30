package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/config"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/ledger"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres"
	pspclient "github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/psp"
	httpiface "github.com/VictorXdAugusto/payment-gateway-go/internal/interfaces/http"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/interfaces/http/handler"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/observability"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/usecase"
)

func main() {
	if err := run(); err != nil {
		slog.Error("aplicação encerrada com erro", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	// ctx é cancelado quando o container recebe SIGTERM/SIGINT (docker stop, Ctrl+C).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Composição: é aqui (e só aqui) que as peças concretas se conhecem.
	txm := postgres.NewTxManager(pool)
	payments := postgres.NewPaymentRepository(txm)
	keys := postgres.NewIdempotencyStore(txm, cfg.IdempotencyLease)
	newID := func() payment.ID { return payment.ID("pay_" + strings.ReplaceAll(uuid.NewString(), "-", "")) }

	metrics := observability.New()
	metrics.RegisterBacklog(postgres.NewStatsRepository(txm), 2*time.Second)
	observability.Serve(ctx, ":"+cfg.MetricsPort, observability.OpsHandler(metrics, pool), logger)

	rawGateway := pspclient.New(pspclient.Config{
		BaseURL:        cfg.PSPBaseURL,
		AttemptTimeout: cfg.PSPAttemptTimeout,
		MaxAttempts:    cfg.PSPMaxAttempts,
		BaseBackoff:    cfg.PSPBaseBackoff,
		MaxBackoff:     cfg.PSPMaxBackoff,
	})
	gateway := observability.InstrumentGateway(rawGateway, metrics)
	ledgerRepo := postgres.NewLedgerRepository(txm)
	newLedgerTxID := func() ledger.TransactionID {
		return ledger.TransactionID("ltx_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	}

	newEventID := func() string { return "evt_" + strings.ReplaceAll(uuid.NewString(), "-", "") }
	events := usecase.NewEventRecorder(postgres.NewOutboxRepository(txm), newEventID)

	paymentHandler := handler.NewPayment(
		usecase.NewCreatePayment(txm, payments, keys, gateway, events, newID, time.Now),
		usecase.NewCapturePayment(txm, payments, ledgerRepo, keys, gateway, events, cfg.PlatformFeeBps, newLedgerTxID, time.Now),
		usecase.NewGetPayment(payments),
	)

	srv := &http.Server{
		Addr: ":" + cfg.HTTPPort,
		Handler: httpiface.NewRouter(handler.NewHealth(pool), paymentHandler,
			postgres.NewMerchantAuthenticator(pool), metrics),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("servidor iniciado", "addr", srv.Addr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		logger.Info("sinal recebido, encerrando")
	}

	// Graceful shutdown: para de aceitar conexões novas e deixa as em andamento terminarem.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
