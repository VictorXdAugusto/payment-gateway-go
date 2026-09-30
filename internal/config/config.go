// Package config carrega a configuração da aplicação a partir de variáveis de ambiente.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPPort string
	// MetricsPort: porta de operação (/metrics, /health, /ready), separada da API pública.
	MetricsPort     string
	DatabaseURL     string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	// IdempotencyLease: por quanto tempo uma requisição segura a chave antes de outra poder
	// assumir. Deve ser maior que o pior caso de processamento de uma requisição.
	IdempotencyLease time.Duration

	PSPBaseURL        string
	PSPAttemptTimeout time.Duration
	PSPMaxAttempts    int
	PSPBaseBackoff    time.Duration
	PSPMaxBackoff     time.Duration

	// PlatformFeeBps: taxa do gateway em basis points (290 = 2,90%).
	PlatformFeeBps int64
	// UnknownGrace: quanto esperar antes de concluir que o PSP nunca recebeu uma tentativa.
	UnknownGrace time.Duration
}

// Load lê o ambiente e falha cedo se algo obrigatório estiver faltando.
func Load() (Config, error) {
	cfg := Config{
		HTTPPort:        getEnv("HTTP_PORT", "8080"),
		MetricsPort:     getEnv("METRICS_PORT", "9102"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		ShutdownTimeout: 15 * time.Second,
	}

	lease, err := time.ParseDuration(getEnv("IDEMPOTENCY_LEASE", "30s"))
	if err != nil || lease <= 0 {
		return Config{}, fmt.Errorf("IDEMPOTENCY_LEASE inválido: %q", os.Getenv("IDEMPOTENCY_LEASE"))
	}
	cfg.IdempotencyLease = lease

	cfg.PSPBaseURL = getEnv("PSP_BASE_URL", "http://localhost:9090")
	if cfg.PSPAttemptTimeout, err = envDuration("PSP_ATTEMPT_TIMEOUT", "2s"); err != nil {
		return Config{}, err
	}
	if cfg.PSPBaseBackoff, err = envDuration("PSP_BASE_BACKOFF", "100ms"); err != nil {
		return Config{}, err
	}
	if cfg.PSPMaxBackoff, err = envDuration("PSP_MAX_BACKOFF", "1s"); err != nil {
		return Config{}, err
	}
	if cfg.UnknownGrace, err = envDuration("UNKNOWN_GRACE", "2m"); err != nil {
		return Config{}, err
	}
	if cfg.PSPMaxAttempts, err = envInt("PSP_MAX_ATTEMPTS", 3, 1, 10); err != nil {
		return Config{}, err
	}
	fee, err := envInt("PLATFORM_FEE_BPS", 290, 0, 10000)
	if err != nil {
		return Config{}, err
	}
	cfg.PlatformFeeBps = int64(fee)

	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL é obrigatória")
	}

	level, err := parseLevel(getEnv("LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = level

	return cfg, nil
}

func envDuration(key, fallback string) (time.Duration, error) {
	d, err := time.ParseDuration(getEnv(key, fallback))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s inválido: %q", key, os.Getenv(key))
	}
	return d, nil
}

func envInt(key string, fallback, min, max int) (int, error) {
	v := fallback
	if raw := os.Getenv(key); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return 0, fmt.Errorf("%s inválido: %q", key, raw)
		}
		v = n
	}
	if v < min || v > max {
		return 0, fmt.Errorf("%s fora do intervalo [%d, %d]: %d", key, min, max, v)
	}
	return v, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(s))); err != nil {
		return 0, fmt.Errorf("LOG_LEVEL inválido %q: %w", s, err)
	}
	return l, nil
}

// WorkerConfig é a configuração do processo de entrega de webhooks (cmd/worker).
type WorkerConfig struct {
	DatabaseURL string
	MetricsPort string // porta de operação do worker (/metrics, /health, /ready)
	LogLevel    slog.Level

	BatchSize    int
	Concurrency  int
	PollInterval time.Duration
	Lease        time.Duration // deve ser MAIOR que o timeout de uma entrega
	MaxAttempts  int
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration

	DeliveryTimeout time.Duration
	// AllowPrivate desliga a proteção contra SSRF. SÓ desenvolvimento.
	AllowPrivate bool

	// Reconciliação de pagamentos presos (unknown e created parado).
	PSPBaseURL          string
	PSPAttemptTimeout   time.Duration
	PSPMaxAttempts      int
	PSPBaseBackoff      time.Duration
	PSPMaxBackoff       time.Duration
	UnknownGrace        time.Duration
	ReconcileInterval   time.Duration
	ReconcileStaleAfter time.Duration
	ReconcilePageSize   int
	ReconcileMaxErrors  int

	// Retenção: quanto tempo guardar o que já não serve para operar.
	RetentionInterval    time.Duration
	RetentionBatch       int
	IdempotencyRetention time.Duration
	OutboxRetention      time.Duration
}

func LoadWorker() (WorkerConfig, error) {
	c := WorkerConfig{DatabaseURL: os.Getenv("DATABASE_URL"), MetricsPort: getEnv("METRICS_PORT", "9103")}
	if c.DatabaseURL == "" {
		return WorkerConfig{}, errors.New("DATABASE_URL é obrigatória")
	}
	var err error
	if c.LogLevel, err = parseLevel(getEnv("LOG_LEVEL", "info")); err != nil {
		return WorkerConfig{}, err
	}
	if c.PollInterval, err = envDuration("WEBHOOK_POLL_INTERVAL", "500ms"); err != nil {
		return WorkerConfig{}, err
	}
	if c.Lease, err = envDuration("WEBHOOK_LEASE", "60s"); err != nil {
		return WorkerConfig{}, err
	}
	if c.BaseBackoff, err = envDuration("WEBHOOK_BASE_BACKOFF", "10s"); err != nil {
		return WorkerConfig{}, err
	}
	if c.MaxBackoff, err = envDuration("WEBHOOK_MAX_BACKOFF", "1h"); err != nil {
		return WorkerConfig{}, err
	}
	if c.DeliveryTimeout, err = envDuration("WEBHOOK_TIMEOUT", "5s"); err != nil {
		return WorkerConfig{}, err
	}
	if c.BatchSize, err = envInt("WEBHOOK_BATCH", 20, 1, 500); err != nil {
		return WorkerConfig{}, err
	}
	if c.Concurrency, err = envInt("WEBHOOK_CONCURRENCY", 8, 1, 200); err != nil {
		return WorkerConfig{}, err
	}
	if c.MaxAttempts, err = envInt("WEBHOOK_MAX_ATTEMPTS", 10, 1, 100); err != nil {
		return WorkerConfig{}, err
	}
	c.AllowPrivate = getEnv("WEBHOOK_ALLOW_PRIVATE", "false") == "true"

	c.PSPBaseURL = getEnv("PSP_BASE_URL", "http://localhost:9090")
	if c.PSPAttemptTimeout, err = envDuration("PSP_ATTEMPT_TIMEOUT", "2s"); err != nil {
		return WorkerConfig{}, err
	}
	if c.PSPBaseBackoff, err = envDuration("PSP_BASE_BACKOFF", "100ms"); err != nil {
		return WorkerConfig{}, err
	}
	if c.PSPMaxBackoff, err = envDuration("PSP_MAX_BACKOFF", "1s"); err != nil {
		return WorkerConfig{}, err
	}
	if c.PSPMaxAttempts, err = envInt("PSP_MAX_ATTEMPTS", 3, 1, 10); err != nil {
		return WorkerConfig{}, err
	}
	if c.UnknownGrace, err = envDuration("UNKNOWN_GRACE", "2m"); err != nil {
		return WorkerConfig{}, err
	}
	if c.ReconcileInterval, err = envDuration("RECONCILE_INTERVAL", "30s"); err != nil {
		return WorkerConfig{}, err
	}
	if c.ReconcileStaleAfter, err = envDuration("RECONCILE_STALE_AFTER", "1m"); err != nil {
		return WorkerConfig{}, err
	}
	if c.ReconcilePageSize, err = envInt("RECONCILE_PAGE_SIZE", 100, 1, 1000); err != nil {
		return WorkerConfig{}, err
	}
	if c.ReconcileMaxErrors, err = envInt("RECONCILE_MAX_CONSECUTIVE_ERRORS", 5, 1, 100); err != nil {
		return WorkerConfig{}, err
	}
	if c.RetentionInterval, err = envDuration("RETENTION_INTERVAL", "1h"); err != nil {
		return WorkerConfig{}, err
	}
	if c.RetentionBatch, err = envInt("RETENTION_BATCH", 1000, 1, 100000); err != nil {
		return WorkerConfig{}, err
	}
	if c.IdempotencyRetention, err = envDuration("IDEMPOTENCY_RETENTION", "24h"); err != nil {
		return WorkerConfig{}, err
	}
	if c.OutboxRetention, err = envDuration("OUTBOX_RETENTION", "168h"); err != nil {
		return WorkerConfig{}, err
	}

	// Uma requisição viva pode demorar até tentativas x timeout no PSP. Reconciliar antes disso
	// atropelaria uma requisição que ainda vai gravar o próprio desfecho.
	if worst := time.Duration(c.PSPMaxAttempts) * c.PSPAttemptTimeout; c.ReconcileStaleAfter <= worst {
		return WorkerConfig{}, fmt.Errorf("RECONCILE_STALE_AFTER (%s) deve ser maior que PSP_MAX_ATTEMPTS x PSP_ATTEMPT_TIMEOUT (%s)",
			c.ReconcileStaleAfter, worst)
	}

	// Se o lease fosse menor que o timeout, outro worker reassumiria uma entrega AINDA em curso.
	if c.Lease <= c.DeliveryTimeout {
		return WorkerConfig{}, fmt.Errorf("WEBHOOK_LEASE (%s) deve ser maior que WEBHOOK_TIMEOUT (%s)", c.Lease, c.DeliveryTimeout)
	}
	return c, nil
}
