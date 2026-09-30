package http_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/idempotency"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/postgres/pgtest"
	httpiface "github.com/VictorXdAugusto/payment-gateway-go/internal/interfaces/http"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/interfaces/http/handler"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/usecase"
)

type api struct {
	t      *testing.T
	srv    *httptest.Server
	pool   *pgxpool.Pool
	keys   idempotency.Store
	apiKey string
	merch  string
}

func newAPI(t *testing.T) *api {
	t.Helper()
	pool := pgtest.New(t)
	ctx := context.Background()
	txm := postgres.NewTxManager(pool)
	payments := postgres.NewPaymentRepository(txm)
	keys := postgres.NewIdempotencyStore(txm, 30*time.Second)

	var seq atomic.Int64
	newID := func() payment.ID { return payment.ID(fmt.Sprintf("pay_%d", seq.Add(1))) }

	h := handler.NewPayment(
		usecase.NewCreatePayment(txm, payments, keys, newID, time.Now),
		usecase.NewGetPayment(payments),
	)
	srv := httptest.NewServer(httpiface.NewRouter(handler.NewHealth(pool), h, postgres.NewMerchantAuthenticator(pool)))
	t.Cleanup(srv.Close)

	a := &api{t: t, srv: srv, pool: pool, keys: keys}
	a.apiKey, a.merch = a.newMerchant(ctx, "loja")
	return a
}

func (a *api) newMerchant(ctx context.Context, name string) (apiKey, id string) {
	a.t.Helper()
	apiKey = "sk_test_" + name
	err := a.pool.QueryRow(ctx,
		`INSERT INTO merchants (name, api_key_hash) VALUES ($1, $2) RETURNING id::text`,
		name, postgres.HashAPIKey(apiKey)).Scan(&id)
	if err != nil {
		a.t.Fatal(err)
	}
	return apiKey, id
}

type resp struct {
	status int
	header stdhttp.Header
	body   string
}

func (a *api) do(method, path, apiKey, idemKey, body string) resp {
	a.t.Helper()
	req, err := stdhttp.NewRequest(method, a.srv.URL+path, strings.NewReader(body))
	if err != nil {
		a.t.Fatal(err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	res, err := stdhttp.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, header: res.Header, body: string(b)}
}

func (a *api) create(idemKey, body string) resp {
	a.t.Helper()
	return a.do("POST", "/v1/payments", a.apiKey, idemKey, body)
}

func errorCode(t *testing.T, r resp) string {
	t.Helper()
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.body), &e); err != nil {
		t.Fatalf("corpo não é o envelope de erro: %q", r.body)
	}
	return e.Error.Code
}

func TestAuth(t *testing.T) {
	a := newAPI(t)
	tests := []struct {
		name, key string
	}{
		{"sem header", ""},
		{"key desconhecida", "sk_test_nao_existe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := a.do("POST", "/v1/payments", tt.key, "k", `{"amount":100,"currency":"BRL"}`)
			if r.status != 401 || errorCode(t, r) != "unauthorized" || r.header.Get("WWW-Authenticate") == "" {
				t.Errorf("status=%d body=%s", r.status, r.body)
			}
		})
	}

	req, _ := stdhttp.NewRequest("GET", a.srv.URL+"/v1/payments/x", nil)
	req.Header.Set("Authorization", "Basic abc")
	res, _ := stdhttp.DefaultClient.Do(req)
	if res.StatusCode != 401 {
		t.Errorf("esquema Basic deveria ser 401, veio %d", res.StatusCode)
	}
	if r := a.do("GET", "/health", "", "", ""); r.status != 200 {
		t.Errorf("/health é aberto para o orquestrador, veio %d", r.status)
	}
}

func TestCreate_Then_RetryReplays(t *testing.T) {
	a := newAPI(t)
	body := `{"amount":5000,"currency":"BRL"}`

	first := a.create("key-1", body)
	if first.status != 201 || first.header.Get("Idempotent-Replayed") != "" || first.header.Get("X-Request-Id") == "" {
		t.Fatalf("primeira: status=%d headers=%v body=%s", first.status, first.header, first.body)
	}
	var view usecase.PaymentView
	if err := json.Unmarshal([]byte(first.body), &view); err != nil || view.Status != "created" || view.Amount != 5000 {
		t.Fatalf("corpo: %s (%v)", first.body, err)
	}

	retry := a.create("key-1", body)
	if retry.status != 201 || retry.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("retry: status=%d replayed=%q", retry.status, retry.header.Get("Idempotent-Replayed"))
	}
	if retry.body != first.body {
		t.Errorf("o replay precisa devolver exatamente o mesmo corpo:\n1ª: %s\n2ª: %s", first.body, retry.body)
	}

	// Whitespace e ordem das chaves do JSON não mudam a requisição.
	if r := a.create("key-1", `{ "currency": "BRL",   "amount": 5000 }`); r.status != 201 || r.header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("JSON equivalente deveria ser replay: status=%d body=%s", r.status, r.body)
	}
}

func TestCreate_IdempotencyErrors(t *testing.T) {
	a := newAPI(t)
	if r := a.create("key-1", `{"amount":5000,"currency":"BRL"}`); r.status != 201 {
		t.Fatal(r)
	}

	t.Run("mesma chave, corpo diferente: 422", func(t *testing.T) {
		r := a.create("key-1", `{"amount":9999,"currency":"BRL"}`)
		if r.status != 422 || errorCode(t, r) != "idempotency_key_reuse" {
			t.Errorf("status=%d body=%s", r.status, r.body)
		}
	})

	t.Run("chave em andamento: 409 com Retry-After", func(t *testing.T) {
		hash := idempotency.Fingerprint("create_payment", "1234", "BRL")
		if _, err := a.keys.Acquire(context.Background(), idempotency.Key{MerchantID: a.merch, Value: "busy"}, hash); err != nil {
			t.Fatal(err)
		}
		r := a.create("busy", `{"amount":1234,"currency":"BRL"}`)
		if r.status != 409 || errorCode(t, r) != "idempotency_key_in_use" || r.header.Get("Retry-After") == "" {
			t.Errorf("status=%d retry-after=%q body=%s", r.status, r.header.Get("Retry-After"), r.body)
		}
	})

	t.Run("sem header: 400", func(t *testing.T) {
		r := a.create("", `{"amount":100,"currency":"BRL"}`)
		if r.status != 400 || errorCode(t, r) != "missing_idempotency_key" {
			t.Errorf("status=%d body=%s", r.status, r.body)
		}
	})

	t.Run("chave inválida: 400", func(t *testing.T) {
		r := a.create(strings.Repeat("k", 256), `{"amount":100,"currency":"BRL"}`)
		if r.status != 400 || errorCode(t, r) != "invalid_idempotency_key" {
			t.Errorf("status=%d body=%s", r.status, r.body)
		}
	})
}

func TestCreate_InvalidBodies_Return400_AndDoNotConsumeTheKey(t *testing.T) {
	a := newAPI(t)
	bad := map[string]string{
		"json quebrado":      `{"amount":`,
		"vazio":              ``,
		"campo desconhecido": `{"amount":100,"currency":"BRL","extra":1}`,
		"lixo depois":        `{"amount":100,"currency":"BRL"} {"x":1}`,
		"amount ausente":     `{"currency":"BRL"}`,
		"amount zero":        `{"amount":0,"currency":"BRL"}`,
		"amount negativo":    `{"amount":-5,"currency":"BRL"}`,
		"amount texto":       `{"amount":"100","currency":"BRL"}`,
		"amount decimal":     `{"amount":10.5,"currency":"BRL"}`,
		"moeda inválida":     `{"amount":100,"currency":"XYZ"}`,
		"moeda ausente":      `{"amount":100}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			r := a.create("same-key", body)
			if r.status != 400 || errorCode(t, r) != "invalid_request" {
				t.Errorf("status=%d body=%s", r.status, r.body)
			}
		})
	}

	// Depois de tantos erros, a chave continua livre.
	if r := a.create("same-key", `{"amount":100,"currency":"BRL"}`); r.status != 201 {
		t.Fatalf("a chave foi consumida por requisição inválida: %d %s", r.status, r.body)
	}
}

func TestCreate_OversizedBody_Returns400(t *testing.T) {
	a := newAPI(t)
	huge := `{"amount":100,"currency":"BRL","x":"` + strings.Repeat("a", 2<<20) + `"}`
	if r := a.create("big", huge); r.status != 400 {
		t.Errorf("status = %d, want 400", r.status)
	}
}

func TestGet(t *testing.T) {
	a := newAPI(t)
	created := a.create("k", `{"amount":777,"currency":"USD"}`)
	var view usecase.PaymentView
	_ = json.Unmarshal([]byte(created.body), &view)

	t.Run("próprio pagamento", func(t *testing.T) {
		r := a.do("GET", "/v1/payments/"+view.ID, a.apiKey, "", "")
		if r.status != 200 || r.body != created.body {
			t.Errorf("status=%d\n get: %s\n post: %s", r.status, r.body, created.body)
		}
	})
	t.Run("de outro lojista é 404", func(t *testing.T) {
		otherKey, _ := a.newMerchant(context.Background(), "intrusa")
		if r := a.do("GET", "/v1/payments/"+view.ID, otherKey, "", ""); r.status != 404 || errorCode(t, r) != "not_found" {
			t.Errorf("status=%d body=%s", r.status, r.body)
		}
	})
	t.Run("inexistente é 404", func(t *testing.T) {
		if r := a.do("GET", "/v1/payments/nao_existe", a.apiKey, "", ""); r.status != 404 {
			t.Errorf("status=%d", r.status)
		}
	})
}

func TestRoutes_UnknownAndWrongMethod(t *testing.T) {
	a := newAPI(t)
	if r := a.do("GET", "/v1/nada", a.apiKey, "", ""); r.status != 404 {
		t.Errorf("rota inexistente: %d", r.status)
	}
	if r := a.do("DELETE", "/v1/payments", a.apiKey, "", ""); r.status != 405 {
		t.Errorf("método errado: %d", r.status)
	}
}
