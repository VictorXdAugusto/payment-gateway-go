// Package http monta o servidor HTTP: rotas e middlewares.
package http

import (
	"net/http"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/interfaces/http/handler"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/interfaces/http/middleware"
)

// NewRouter registra as rotas usando o ServeMux da stdlib (Go 1.22+ aceita método no padrão).
// /health e /ready ficam abertos (o orquestrador não tem API key); /v1/* exige autenticação.
//
// recorder recebe a medição de cada requisição (nil = sem métricas).
func NewRouter(health *handler.Health, payments *handler.Payment, account *handler.Account, auth middleware.Authenticator, recorder middleware.RequestRecorder) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", health.Live)
	mux.HandleFunc("GET /ready", health.Ready)

	authed := middleware.Auth(auth)
	mux.Handle("POST /v1/payments", authed(http.HandlerFunc(payments.Create)))
	mux.Handle("POST /v1/payments/{id}/capture", authed(http.HandlerFunc(payments.Capture)))
	mux.Handle("POST /v1/payments/{id}/void", authed(http.HandlerFunc(payments.Void)))
	mux.Handle("POST /v1/payments/{id}/refund", authed(http.HandlerFunc(payments.Refund)))
	mux.Handle("GET /v1/balance", authed(http.HandlerFunc(account.Balance)))
	mux.Handle("GET /v1/statement", authed(http.HandlerFunc(account.Statement)))
	mux.Handle("GET /v1/payments/{id}", authed(http.HandlerFunc(payments.Get)))

	return middleware.ObserveWith(recorder, mux)
}
