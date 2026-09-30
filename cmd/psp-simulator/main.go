// Comando psp-simulator sobe o adquirente falso (só para desenvolvimento).
package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/pspsim"
)

func main() {
	addr := ":" + envOr("PSP_PORT", "9090")
	slow, err := time.ParseDuration(envOr("PSP_SLOW", "3s"))
	if err != nil {
		slog.Error("PSP_SLOW inválido", "error", err)
		os.Exit(1)
	}

	slog.Info("psp-simulator no ar", "addr", addr, "slow", slow.String(), "valores_magicos", pspsim.Describe())
	srv := &http.Server{Addr: addr, Handler: pspsim.New(slow).Handler(), ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("psp-simulator encerrou", "error", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
