package observability

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Pinger é o que o /ready precisa do banco.
type Pinger interface {
	Ping(ctx context.Context) error
}

// OpsHandler serve /metrics, /health (vivo) e /ready (banco acessível). Fica numa porta
// SEPARADA da API pública: métricas revelam volume e erros e não devem ser expostas ao mundo.
func OpsHandler(m *Metrics, db Pinger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	return mux
}

// Serve sobe o servidor de operação e o desliga quando ctx é cancelado. Falha ao escutar é
// só registrada: perder a observabilidade não pode derrubar o processamento de pagamentos.
func Serve(ctx context.Context, addr string, h http.Handler, log *slog.Logger) {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		log.Info("servidor de operação iniciado", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("servidor de operação falhou", "addr", addr, "error", err)
		}
	}()
}
