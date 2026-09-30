// Package middleware reúne os middlewares HTTP: request id, log, recover e autenticação.
package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
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

// Authenticator troca uma API key pelo id do lojista.
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
			if err != nil {
				unauthorized(w)
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

// Observe gera o request id, registra a requisição e transforma pânico em 500
// (um handler com bug não derruba o processo nem deixa o cliente sem resposta).
func Observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", id)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
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
			slog.Info("requisição", "method", r.Method, "path", r.URL.Path, "status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(), "request_id", id)
		}()

		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}
