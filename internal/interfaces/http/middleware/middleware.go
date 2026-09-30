// Package middleware reúne os middlewares HTTP: request id, log, recover e autenticação.
package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/auth"
)

type ctxKey int

const (
	merchantKey ctxKey = iota
	requestIDKey
)

// MerchantID devolve o lojista autenticado da requisição ("" se não houver).
func MerchantID(ctx context.Context) string {
	v, _ := ctx.Value(merchantKey).(string)
	return v
}

// Authenticator troca uma API key pelo id do lojista. Deve devolver auth.ErrUnauthorized para
// credencial inválida; qualquer outro erro é tratado como indisponibilidade (503).
type Authenticator interface {
	Authenticate(ctx context.Context, apiKey string) (string, error)
}

// Auth exige "Authorization: Bearer <api key>".
func Auth(a Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || token == "" {
				unauthorized(w)
				return
			}
			merchantID, err := a.Authenticate(r.Context(), token)
			switch {
			case errors.Is(err, auth.ErrUnauthorized):
				unauthorized(w)
				return
			case err != nil:
				slog.ErrorContext(r.Context(), "falha ao autenticar", "error", err)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":{"code":"auth_unavailable","message":"não foi possível validar a credencial agora; tente novamente"}}` + "\n"))
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), merchantKey, merchantID)))
		})
	}
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"api key ausente ou inválida"}}` + "\n"))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// RequestRecorder recebe uma medição por requisição (métricas). route é o PADRÃO do mux que
// atendeu ("POST /v1/payments"), nunca o caminho cru: caminhos com id criariam uma série por
// pagamento. Requisição que não casou com rota nenhuma chega como "unmatched".
type RequestRecorder interface {
	HTTPRequest(method, route string, status int, d time.Duration)
}

// Observe gera o request id, registra a requisição e transforma pânico em 500
// (um handler com bug não derruba o processo nem deixa o cliente sem resposta).
func Observe(next http.Handler) http.Handler { return ObserveWith(nil, next) }

// ObserveWith é Observe mais a medição de cada requisição em rec (nil = sem métricas).
func ObserveWith(recorder RequestRecorder, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", id)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		inner := r.WithContext(context.WithValue(r.Context(), requestIDKey, id))
		start := time.Now()

		defer func() {
			if p := recover(); p != nil {
				slog.Error("pânico no handler", "panic", p, "request_id", id, "path", r.URL.Path)
				if rec.status == http.StatusOK {
					rec.Header().Set("Content-Type", "application/json")
					rec.WriteHeader(http.StatusInternalServerError)
					_, _ = rec.Write([]byte(`{"error":{"code":"internal_error","message":"erro interno"}}` + "\n"))
				}
			}
			elapsed := time.Since(start)
			route := inner.Pattern // o mux preenche no request que ele recebe
			if route == "" {
				route = "unmatched"
			}
			if recorder != nil {
				recorder.HTTPRequest(r.Method, route, rec.status, elapsed)
			}
			slog.Info("requisição", "method", r.Method, "path", r.URL.Path, "route", route, "status", rec.status,
				"duration_ms", elapsed.Milliseconds(), "request_id", id)
		}()

		next.ServeHTTP(rec, inner)
	})
}
