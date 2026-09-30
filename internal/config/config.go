// Package config carrega a configuração da aplicação a partir de variáveis de ambiente.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

type Config struct {
	HTTPPort        string
	DatabaseURL     string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	// IdempotencyLease: por quanto tempo uma requisição segura a chave antes de outra poder
	// assumir. Deve ser maior que o pior caso de processamento de uma requisição.
	IdempotencyLease time.Duration
}

// Load lê o ambiente e falha cedo se algo obrigatório estiver faltando.
func Load() (Config, error) {
	cfg := Config{
		HTTPPort:        getEnv("HTTP_PORT", "8080"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		ShutdownTimeout: 15 * time.Second,
	}

	lease, err := time.ParseDuration(getEnv("IDEMPOTENCY_LEASE", "30s"))
	if err != nil || lease <= 0 {
		return Config{}, fmt.Errorf("IDEMPOTENCY_LEASE inválido: %q", os.Getenv("IDEMPOTENCY_LEASE"))
	}
	cfg.IdempotencyLease = lease

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
