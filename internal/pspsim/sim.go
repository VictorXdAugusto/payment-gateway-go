// Package pspsim é um adquirente FALSO para desenvolvimento e testes. Roda como serviço HTTP
// de verdade para exercitar timeout, retry e falha parcial, e se comporta como um PSP
// sério: idempotente por Idempotency-Key.
//
// Como os cartões de teste da Stripe, o VALOR (em centavos) escolhe o comportamento.
package pspsim

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	AmountDeclined      int64 = 4000 // recusa: saldo insuficiente (402)
	AmountSlowApproved  int64 = 5000 // sempre demora além do timeout, MAS APROVA (o caso perigoso)
	AmountSlowLost      int64 = 5001 // demora além do timeout e NÃO chega a aprovar
	AmountFlaky         int64 = 5002 // 503 nas 2 primeiras tentativas de cada chave, depois aprova
	AmountDown          int64 = 5003 // sempre 503 e nunca aprova
	AmountCaptureFlaky  int64 = 6000 // autoriza normal; a 1ª tentativa de captura dá 503
	AmountCaptureDeclin int64 = 6001 // autoriza normal; a captura é recusada (402)
	AmountCaptureDown   int64 = 6002 // autoriza normal; a captura dá sempre 503 e nunca acontece
)

type record struct {
	status int
	body   []byte
}

type authorization struct {
	ID       string
	Amount   int64
	Captured bool
}

type Simulator struct {
	slow time.Duration

	mu        sync.Mutex
	responses map[string]record         // idempotency key -> resposta gravada
	auths     map[string]*authorization // id -> autorização
	byKey     map[string]string         // idempotency key da autorização -> id
	declined  map[string]string         // idempotency key -> código da recusa
	attempts  map[string]int            // tentativas por chave (para o comportamento "flaky")
	seq       int
}

// New cria o simulador. slow é quanto tempo dura uma resposta "lenta".
func New(slow time.Duration) *Simulator {
	return &Simulator{
		slow: slow, responses: map[string]record{}, auths: map[string]*authorization{},
		byKey: map[string]string{}, declined: map[string]string{}, attempts: map[string]int{},
	}
}

func (s *Simulator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { write(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /v1/authorizations", s.authorize)
	mux.HandleFunc("POST /v1/authorizations/{id}/capture", s.capture)
	mux.HandleFunc("GET /v1/authorizations", s.lookup)
	return mux
}

// Authorizations devolve quantas autorizações DISTINTAS existem (para os testes provarem
// que retries não autorizam duas vezes).
func (s *Simulator) Authorizations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.auths)
}

func (s *Simulator) attempt(key string) int {
	s.attempts[key]++
	return s.attempts[key]
}

func (s *Simulator) authorize(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	var req struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	if key == "" || json.NewDecoder(r.Body).Decode(&req) != nil {
		write(w, 400, errBody("bad_request", "Idempotency-Key e corpo JSON são obrigatórios"))
		return
	}

	s.mu.Lock()
	if rec, ok := s.responses[key]; ok { // idempotência: mesma chave, mesma resposta
		s.mu.Unlock()
		if req.Amount == AmountSlowApproved {
			s.sleep(r) // continua lento no replay: o cliente nunca chega a saber do desfecho
		}
		writeRaw(w, rec)
		return
	}
	n := s.attempt(key)

	switch {
	case req.Amount == AmountDown:
		s.mu.Unlock()
		write(w, 503, errBody("unavailable", "PSP indisponível"))
		return
	case req.Amount == AmountFlaky && n <= 2:
		s.mu.Unlock()
		write(w, 503, errBody("unavailable", "PSP instável"))
		return
	case req.Amount == AmountDeclined:
		rec := s.save(key, 402, errBody("insufficient_funds", "saldo insuficiente"))
		s.declined[key] = "insufficient_funds"
		s.mu.Unlock()
		writeRaw(w, rec)
		return
	case req.Amount == AmountSlowLost:
		s.mu.Unlock()
		s.sleep(r)
		write(w, 503, errBody("timeout", "não processado")) // ninguém mais está ouvindo
		return
	}

	// Aprova: grava ANTES de qualquer espera. No caso "lento" a autorização existe mesmo
	// que o cliente já tenha desistido de esperar.
	s.seq++
	a := &authorization{ID: fmt.Sprintf("auth_%d", s.seq), Amount: req.Amount}
	s.auths[a.ID], s.byKey[key] = a, a.ID
	rec := s.save(key, 201, map[string]string{"id": a.ID, "status": "authorized"})
	s.mu.Unlock()

	if req.Amount == AmountSlowApproved {
		s.sleep(r)
	}
	writeRaw(w, rec)
}

func (s *Simulator) capture(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	var req struct {
		Amount int64 `json:"amount"`
	}
	if key == "" || json.NewDecoder(r.Body).Decode(&req) != nil {
		write(w, 400, errBody("bad_request", "Idempotency-Key e corpo JSON são obrigatórios"))
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.responses[key]; ok {
		writeRaw(w, rec)
		return
	}
	a, ok := s.auths[r.PathValue("id")]
	if !ok {
		write(w, 404, errBody("not_found", "autorização inexistente"))
		return
	}
	n := s.attempt(key)

	switch {
	case a.Amount == AmountCaptureFlaky && n == 1:
		write(w, 503, errBody("unavailable", "PSP instável"))
		return
	case a.Amount == AmountCaptureDown:
		write(w, 503, errBody("unavailable", "PSP indisponível"))
		return
	case a.Amount == AmountCaptureDeclin:
		writeRaw(w, s.save(key, 402, errBody("capture_declined", "captura recusada")))
		return
	case req.Amount != a.Amount:
		writeRaw(w, s.save(key, 422, errBody("amount_mismatch", "valor diferente do autorizado")))
		return
	}
	a.Captured = true
	writeRaw(w, s.save(key, 200, map[string]string{"id": a.ID, "status": "captured"}))
}

func (s *Simulator) lookup(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("idempotency_key")
	s.mu.Lock()
	defer s.mu.Unlock()

	if id, ok := s.byKey[key]; ok {
		write(w, 200, map[string]string{"id": id, "status": "authorized"})
		return
	}
	if code, ok := s.declined[key]; ok {
		write(w, 200, map[string]string{"status": "declined", "decline_code": code})
		return
	}
	write(w, 404, errBody("not_found", "o PSP não conhece esta chave"))
}

// save grava a resposta da chave (chamar com s.mu travado).
func (s *Simulator) save(key string, status int, body any) record {
	b, _ := json.Marshal(body)
	rec := record{status: status, body: append(b, '\n')}
	s.responses[key] = rec
	return rec
}

func (s *Simulator) sleep(r *http.Request) {
	select {
	case <-time.After(s.slow):
	case <-r.Context().Done():
	}
}

func errBody(code, msg string) map[string]any {
	return map[string]any{"error": map[string]string{"code": code, "message": msg}}
}

func write(w http.ResponseWriter, status int, body any) {
	b, _ := json.Marshal(body)
	writeRaw(w, record{status: status, body: append(b, '\n')})
}

func writeRaw(w http.ResponseWriter, rec record) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rec.status)
	_, _ = w.Write(rec.body)
}

// Describe lista os comportamentos mágicos (usado no log de boot do simulador).
func Describe() string {
	return strings.Join([]string{
		fmt.Sprintf("%d=recusa", AmountDeclined),
		fmt.Sprintf("%d=lento+aprova", AmountSlowApproved),
		fmt.Sprintf("%d=lento+perde", AmountSlowLost),
		fmt.Sprintf("%d=instável(2x503)", AmountFlaky),
		fmt.Sprintf("%d=fora do ar", AmountDown),
		fmt.Sprintf("%d=captura instável", AmountCaptureFlaky),
		fmt.Sprintf("%d=captura recusada", AmountCaptureDeclin),
		fmt.Sprintf("%d=captura fora do ar", AmountCaptureDown),
	}, ", ")
}
