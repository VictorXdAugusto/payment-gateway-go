// Package webhooksink é um RECEPTOR de webhooks de referência (só desenvolvimento). Mostra o
// que todo lojista precisa fazer ao receber uma entrega:
//
//  1. verificar a assinatura sobre os bytes EXATOS do corpo (antes de parsear);
//  2. rejeitar timestamp fora da tolerância (replay);
//  3. deduplicar pelo id do evento (a entrega é at-least-once);
//  4. responder 2xx rápido.
package webhooksink

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/webhook"
)

const tolerance = 5 * time.Minute

type Received struct {
	EventID  string          `json:"event_id"`
	Type     string          `json:"type"`
	Attempts int             `json:"attempts"` // quantas vezes o gateway bateu aqui para este evento
	Body     json.RawMessage `json:"body"`
}

type Sink struct {
	secret    string
	failFirst int // as N primeiras tentativas de cada evento respondem 500 (para exercitar o retry)
	now       func() time.Time

	mu       sync.Mutex
	attempts map[string]int
	events   map[string]*Received
	order    []string
}

func New(secret string, failFirst int) *Sink {
	return &Sink{secret: secret, failFirst: failFirst, now: time.Now,
		attempts: map[string]int{}, events: map[string]*Received{}}
}

func (s *Sink) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /hook", s.hook)
	mux.HandleFunc("GET /received", s.list)
	return mux
}

func (s *Sink) hook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "corpo ilegível", http.StatusBadRequest)
		return
	}
	if err := webhook.Verify(s.secret, r.Header.Get(webhook.SignatureHeader), body, s.now(), tolerance); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized) // não processa nada sem assinatura válida
		return
	}

	var evt struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if json.Unmarshal(body, &evt) != nil || evt.ID == "" {
		http.Error(w, "evento sem id", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts[evt.ID]++
	n := s.attempts[evt.ID]

	if n <= s.failFirst {
		http.Error(w, "falha simulada", http.StatusInternalServerError)
		return
	}
	if got, dup := s.events[evt.ID]; dup { // já processado: confirma e não processa de novo
		got.Attempts = n
		w.WriteHeader(http.StatusOK)
		return
	}
	s.events[evt.ID] = &Received{EventID: evt.ID, Type: evt.Type, Attempts: n, Body: append(json.RawMessage(nil), body...)}
	s.order = append(s.order, evt.ID)
	w.WriteHeader(http.StatusOK)
}

func (s *Sink) list(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Received, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, *s.events[id])
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
