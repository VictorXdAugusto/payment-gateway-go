// Package handler contém os handlers HTTP.
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Pinger é o que o readiness precisa do banco. Declarada aqui (lado de quem usa)
// para o handler não depender do pgx e ser trivial de testar.
type Pinger interface {
	Ping(ctx context.Context) error
}

type Health struct {
	db Pinger
}

func NewHealth(db Pinger) *Health {
	return &Health{db: db}
}

// Live responde se o processo está de pé. Não toca em dependências:
// se o banco cair, reiniciar o container não resolve, então não deve falhar aqui.
func (h *Health) Live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Ready responde se o serviço consegue trabalhar agora (banco acessível).
// O orquestrador usa isto para decidir se manda tráfego para a instância.
func (h *Health) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := h.db.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
