// Comando webhook-sink sobe o receptor de referência (só desenvolvimento).
package main

import (
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/webhooksink"
)

func main() {
	secret := os.Getenv("SINK_SECRET")
	if secret == "" {
		slog.Error("SINK_SECRET é obrigatório (o mesmo webhook_secret do lojista)")
		os.Exit(1)
	}
	failFirst, _ := strconv.Atoi(os.Getenv("SINK_FAIL_FIRST"))
	addr := ":" + envOr("SINK_PORT", "9100")

	slog.Info("webhook-sink no ar", "addr", addr, "fail_first", failFirst)
	srv := &http.Server{Addr: addr, Handler: webhooksink.New(secret, failFirst).Handler(), ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("webhook-sink encerrou", "error", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
