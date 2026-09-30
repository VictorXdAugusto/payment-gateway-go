// Package http monta o servidor HTTP: rotas e middlewares.
package http

import (
	"net/http"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/interfaces/http/handler"
)

// NewRouter registra as rotas usando o ServeMux da stdlib (Go 1.22+ aceita método no padrão).
func NewRouter(health *handler.Health) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", health.Live)
	mux.HandleFunc("GET /ready", health.Ready)

	return mux
}
