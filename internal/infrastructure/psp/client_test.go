package psp_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
	infra "github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/psp"
	domain "github.com/VictorXdAugusto/payment-gateway-go/internal/psp"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/pspsim"
)

func brl(t testing.TB, cents int64) money.Money {
	t.Helper()
	m, err := money.New(cents, money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func fastCfg(url string) infra.Config {
	return infra.Config{BaseURL: url, AttemptTimeout: 100 * time.Millisecond, MaxAttempts: 3,
		BaseBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond}
}

// scripted devolve um servidor que responde conforme o roteiro e registra as chamadas.
type scripted struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []http.Header
	n     atomic.Int32
}

func newScripted(t *testing.T, handle func(n int, w http.ResponseWriter, r *http.Request)) *scripted {
	s := &scripted{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls = append(s.calls, r.Header.Clone())
		s.mu.Unlock()
		handle(int(s.n.Add(1)), w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func TestAuthorize_RetriesTransientErrors_SendingTheSameKeyEveryTime(t *testing.T) {
	s := newScripted(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		if n < 3 {
			reply(w, 503, `{}`)
			return
		}
		reply(w, 201, `{"id":"auth_9","status":"authorized"}`)
	})

	got, err := infra.New(fastCfg(s.srv.URL)).Authorize(context.Background(),
		domain.AuthorizeRequest{IdempotencyKey: "pay_1", Amount: brl(t, 1000)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Reference != "auth_9" || s.n.Load() != 3 {
		t.Errorf("ref=%q tentativas=%d, want auth_9 e 3", got.Reference, s.n.Load())
	}
	for i, h := range s.calls {
		if h.Get("Idempotency-Key") != "pay_1" {
			t.Errorf("tentativa %d sem a mesma Idempotency-Key: %q", i+1, h.Get("Idempotency-Key"))
		}
	}
}

func TestAuthorize_ExhaustedRetries_IsIndeterminate(t *testing.T) {
	s := newScripted(t, func(_ int, w http.ResponseWriter, _ *http.Request) { reply(w, 503, `{}`) })

	_, err := infra.New(fastCfg(s.srv.URL)).Authorize(context.Background(),
		domain.AuthorizeRequest{IdempotencyKey: "k", Amount: brl(t, 1000)})

	if !errors.Is(err, domain.ErrIndeterminate) || errors.Is(err, domain.ErrDeclined) {
		t.Fatalf("err = %v, want ErrIndeterminate", err)
	}
	if s.n.Load() != 3 {
		t.Errorf("tentativas = %d, want 3 (MaxAttempts)", s.n.Load())
	}
}

func TestAuthorize_Decline_IsDefinitive_AndNotRetried(t *testing.T) {
	s := newScripted(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		reply(w, 402, `{"error":{"code":"insufficient_funds","message":"saldo insuficiente"}}`)
	})

	_, err := infra.New(fastCfg(s.srv.URL)).Authorize(context.Background(),
		domain.AuthorizeRequest{IdempotencyKey: "k", Amount: brl(t, 1000)})

	var d *domain.DeclinedError
	if !errors.As(err, &d) || d.Code != "insufficient_funds" || !errors.Is(err, domain.ErrDeclined) {
		t.Fatalf("err = %v", err)
	}
	if errors.Is(err, domain.ErrIndeterminate) {
		t.Error("recusa NÃO é indeterminada")
	}
	if s.n.Load() != 1 {
		t.Errorf("recusa foi repetida %d vezes; recusa é definitiva", s.n.Load())
	}
}

func TestAuthorize_ClientError_IsNeitherDeclinedNorIndeterminate(t *testing.T) {
	s := newScripted(t, func(_ int, w http.ResponseWriter, _ *http.Request) { reply(w, 400, `{"error":"bad"}`) })

	_, err := infra.New(fastCfg(s.srv.URL)).Authorize(context.Background(),
		domain.AuthorizeRequest{IdempotencyKey: "k", Amount: brl(t, 1000)})

	if err == nil || errors.Is(err, domain.ErrIndeterminate) || errors.Is(err, domain.ErrDeclined) {
		t.Fatalf("err = %v: 4xx é bug de contrato, não pode virar unknown nem recusa", err)
	}
	if s.n.Load() != 1 {
		t.Errorf("4xx foi repetido %d vezes", s.n.Load())
	}
}

func TestAuthorize_Timeout_IsIndeterminate(t *testing.T) {
	s := newScripted(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // sem ler o corpo o servidor não detecta a desconexão
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	})

	start := time.Now()
	_, err := infra.New(fastCfg(s.srv.URL)).Authorize(context.Background(),
		domain.AuthorizeRequest{IdempotencyKey: "k", Amount: brl(t, 1000)})

	if !errors.Is(err, domain.ErrIndeterminate) {
		t.Fatalf("err = %v", err)
	}
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("levou %v: o timeout por tentativa não está valendo", took)
	}
}

func TestAuthorize_UnreadableSuccessBody_IsIndeterminate(t *testing.T) {
	s := newScripted(t, func(_ int, w http.ResponseWriter, _ *http.Request) { reply(w, 201, `<html>oops</html>`) })
	_, err := infra.New(fastCfg(s.srv.URL)).Authorize(context.Background(),
		domain.AuthorizeRequest{IdempotencyKey: "k", Amount: brl(t, 1000)})
	if !errors.Is(err, domain.ErrIndeterminate) {
		t.Fatalf("2xx com corpo ilegível pode ter autorizado: err = %v", err)
	}
}

func TestAuthorize_CallerCancellation_IsNotIndeterminate(t *testing.T) {
	s := newScripted(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()

	cfg := fastCfg(s.srv.URL)
	cfg.AttemptTimeout = 5 * time.Second
	_, err := infra.New(cfg).Authorize(ctx, domain.AuthorizeRequest{IdempotencyKey: "k", Amount: brl(t, 1000)})

	if !errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrIndeterminate) {
		t.Fatalf("err = %v: cancelamento do chamador não é 'PSP indeterminado'", err)
	}
}

func TestBackoff_GrowsExponentially_AndIsCapped(t *testing.T) {
	s := newScripted(t, func(_ int, w http.ResponseWriter, _ *http.Request) { reply(w, 503, `{}`) })

	var waits []time.Duration
	cfg := infra.Config{BaseURL: s.srv.URL, AttemptTimeout: time.Second, MaxAttempts: 5,
		BaseBackoff: 100 * time.Millisecond, MaxBackoff: 300 * time.Millisecond}
	c := infra.NewForTest(cfg,
		func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
		func(max time.Duration) time.Duration { return max }, // sem sorte: devolve o teto
	)

	_, _ = c.Authorize(context.Background(), domain.AuthorizeRequest{IdempotencyKey: "k", Amount: brl(t, 1)})

	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond, 300 * time.Millisecond}
	if len(waits) != len(want) {
		t.Fatalf("esperas = %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("espera %d = %v, want %v", i+1, waits[i], want[i])
		}
	}
}

// --- cliente real contra o simulador real (o que roda no docker compose) ---

func simClient(t *testing.T) (*infra.Client, *pspsim.Simulator) {
	t.Helper()
	sim := pspsim.New(400 * time.Millisecond)
	srv := httptest.NewServer(sim.Handler())
	t.Cleanup(srv.Close)
	return infra.New(fastCfg(srv.URL)), sim
}

func authorize(c *infra.Client, key string, cents int64, t *testing.T) (domain.Authorization, error) {
	return c.Authorize(context.Background(), domain.AuthorizeRequest{IdempotencyKey: key, Amount: brl(t, cents)})
}

func TestSimulator_HappyPath_AndIdempotentRetry(t *testing.T) {
	c, sim := simClient(t)
	a1, err := authorize(c, "pay_1", 1000, t)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := authorize(c, "pay_1", 1000, t) // mesma chave: mesma autorização
	if err != nil || a2.Reference != a1.Reference || sim.Authorizations() != 1 {
		t.Fatalf("a1=%v a2=%v auths=%d, err=%v", a1, a2, sim.Authorizations(), err)
	}
	if a3, _ := authorize(c, "pay_2", 1000, t); a3.Reference == a1.Reference {
		t.Error("chaves diferentes precisam gerar autorizações diferentes")
	}
}

func TestSimulator_Declined(t *testing.T) {
	c, sim := simClient(t)
	_, err := authorize(c, "pay_1", pspsim.AmountDeclined, t)
	if !errors.Is(err, domain.ErrDeclined) {
		t.Fatalf("err = %v", err)
	}
	if sim.Authorizations() != 0 {
		t.Error("recusa não cria autorização")
	}
	res, err := c.Lookup(context.Background(), "pay_1")
	if err != nil || res.Outcome != domain.OutcomeDeclined || res.DeclineCode != "insufficient_funds" {
		t.Errorf("lookup = %+v / %v", res, err)
	}
}

func TestSimulator_Flaky_SucceedsThroughRetries(t *testing.T) {
	c, sim := simClient(t)
	if _, err := authorize(c, "pay_1", pspsim.AmountFlaky, t); err != nil {
		t.Fatalf("2 falhas + sucesso cabem em 3 tentativas: %v", err)
	}
	if sim.Authorizations() != 1 {
		t.Errorf("autorizações = %d", sim.Authorizations())
	}
}

// O cenário perigoso: o PSP APROVOU, mas nós nunca soubemos. Os retries não duplicam a
// autorização (idempotência) e o Lookup revela a verdade.
func TestSimulator_SlowApproved_IsIndeterminateButItHappened(t *testing.T) {
	c, sim := simClient(t)
	_, err := authorize(c, "pay_1", pspsim.AmountSlowApproved, t)
	if !errors.Is(err, domain.ErrIndeterminate) {
		t.Fatalf("err = %v, want ErrIndeterminate", err)
	}
	if sim.Authorizations() != 1 {
		t.Fatalf("autorizações = %d: 3 tentativas com a mesma chave devem gerar UMA autorização", sim.Authorizations())
	}
	res, err := c.Lookup(context.Background(), "pay_1")
	if err != nil || res.Outcome != domain.OutcomeAuthorized || res.Reference == "" {
		t.Errorf("lookup = %+v / %v: a reconciliação precisa descobrir que aprovou", res, err)
	}
}

func TestSimulator_SlowLostAndDown_AreIndeterminateAndDidNotHappen(t *testing.T) {
	for name, amount := range map[string]int64{"lento e perdido": pspsim.AmountSlowLost, "fora do ar": pspsim.AmountDown} {
		t.Run(name, func(t *testing.T) {
			c, sim := simClient(t)
			if _, err := authorize(c, "pay_1", amount, t); !errors.Is(err, domain.ErrIndeterminate) {
				t.Fatalf("err = %v", err)
			}
			res, err := c.Lookup(context.Background(), "pay_1")
			if err != nil || res.Outcome != domain.OutcomeNotFound || sim.Authorizations() != 0 {
				t.Errorf("lookup=%+v err=%v auths=%d, want not_found e 0", res, err, sim.Authorizations())
			}
		})
	}
}

func TestSimulator_Capture(t *testing.T) {
	c, _ := simClient(t)
	ctx := context.Background()

	t.Run("normal e idempotente", func(t *testing.T) {
		a, _ := authorize(c, "pay_1", 1000, t)
		req := domain.CaptureRequest{IdempotencyKey: "capture:pay_1", Reference: a.Reference, Amount: brl(t, 1000)}
		if err := c.Capture(ctx, req); err != nil {
			t.Fatal(err)
		}
		if err := c.Capture(ctx, req); err != nil {
			t.Fatalf("repetir a captura com a mesma chave tem que ser seguro: %v", err)
		}
	})
	t.Run("instável resolve por retry", func(t *testing.T) {
		a, _ := authorize(c, "pay_2", pspsim.AmountCaptureFlaky, t)
		req := domain.CaptureRequest{IdempotencyKey: "capture:pay_2", Reference: a.Reference, Amount: brl(t, pspsim.AmountCaptureFlaky)}
		if err := c.Capture(ctx, req); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("recusada", func(t *testing.T) {
		a, _ := authorize(c, "pay_3", pspsim.AmountCaptureDeclin, t)
		req := domain.CaptureRequest{IdempotencyKey: "capture:pay_3", Reference: a.Reference, Amount: brl(t, pspsim.AmountCaptureDeclin)}
		if err := c.Capture(ctx, req); !errors.Is(err, domain.ErrDeclined) {
			t.Fatalf("err = %v", err)
		}
	})
}

// Lacuna achada por mutação: se o cancelamento cai na ÚLTIMA tentativa não há mais sleep
// de backoff para denunciá-lo. Só a checagem explícita evita virar "PSP indeterminado".
func TestAuthorize_CancellationOnTheLastAttempt_IsNotIndeterminate(t *testing.T) {
	s := newScripted(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()

	cfg := fastCfg(s.srv.URL)
	cfg.MaxAttempts = 1
	cfg.AttemptTimeout = 5 * time.Second
	_, err := infra.New(cfg).Authorize(ctx, domain.AuthorizeRequest{IdempotencyKey: "k", Amount: brl(t, 1000)})

	if !errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrIndeterminate) {
		t.Fatalf("err = %v", err)
	}
}

func TestSimulator_Void(t *testing.T) {
	c, _ := simClient(t)
	ctx := context.Background()

	t.Run("cancela e é idempotente", func(t *testing.T) {
		a, _ := authorize(c, "pay_v1", 1000, t)
		req := domain.VoidRequest{IdempotencyKey: "void:pay_v1", Reference: a.Reference}
		if err := c.Void(ctx, req); err != nil {
			t.Fatal(err)
		}
		if err := c.Void(ctx, req); err != nil {
			t.Fatalf("repetir o cancelamento com a mesma chave tem que ser seguro: %v", err)
		}
	})
	t.Run("capturada não cancela: recusa definitiva", func(t *testing.T) {
		a, _ := authorize(c, "pay_v2", 1000, t)
		if err := c.Capture(ctx, domain.CaptureRequest{IdempotencyKey: "capture:pay_v2", Reference: a.Reference, Amount: brl(t, 1000)}); err != nil {
			t.Fatal(err)
		}
		err := c.Void(ctx, domain.VoidRequest{IdempotencyKey: "void:pay_v2", Reference: a.Reference})
		var declined *domain.DeclinedError
		if !errors.As(err, &declined) || declined.Code != "already_captured" {
			t.Fatalf("err = %v, want recusa already_captured", err)
		}
	})
	t.Run("depois de cancelada, captura é recusada", func(t *testing.T) {
		a, _ := authorize(c, "pay_v3", 1000, t)
		if err := c.Void(ctx, domain.VoidRequest{IdempotencyKey: "void:pay_v3", Reference: a.Reference}); err != nil {
			t.Fatal(err)
		}
		err := c.Capture(ctx, domain.CaptureRequest{IdempotencyKey: "capture:pay_v3", Reference: a.Reference, Amount: brl(t, 1000)})
		if !errors.Is(err, domain.ErrDeclined) {
			t.Fatalf("err = %v, want recusa", err)
		}
	})
	t.Run("PSP fora do ar: indeterminado", func(t *testing.T) {
		a, _ := authorize(c, "pay_v4", pspsim.AmountVoidDown, t)
		err := c.Void(ctx, domain.VoidRequest{IdempotencyKey: "void:pay_v4", Reference: a.Reference})
		if !errors.Is(err, domain.ErrIndeterminate) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestSimulator_Refund(t *testing.T) {
	c, sim := simClient(t)
	ctx := context.Background()
	captured := func(key string, cents int64) domain.Authorization {
		a, err := authorize(c, key, cents, t)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Capture(ctx, domain.CaptureRequest{IdempotencyKey: "capture:" + key, Reference: a.Reference, Amount: brl(t, cents)}); err != nil {
			t.Fatal(err)
		}
		return a
	}
	refund := func(a domain.Authorization, key string, cents int64) error {
		return c.Refund(ctx, domain.RefundRequest{IdempotencyKey: key, Reference: a.Reference, Amount: brl(t, cents)})
	}

	t.Run("parcial, idempotente e sem passar do capturado", func(t *testing.T) {
		a := captured("pay_r1", 1000)
		if err := refund(a, "refund:1", 400); err != nil {
			t.Fatal(err)
		}
		if err := refund(a, "refund:1", 400); err != nil { // retry: não devolve 2x
			t.Fatal(err)
		}
		if err := refund(a, "refund:2", 600); err != nil { // 400 + 600 = 1000: exatamente o capturado
			t.Fatal(err)
		}
		err := refund(a, "refund:3", 1) // 1000 + 1: o PSP impõe o limite
		var declined *domain.DeclinedError
		if !errors.As(err, &declined) || declined.Code != "refund_exceeds_captured" {
			t.Fatalf("err = %v, want refund_exceeds_captured (o retry não pode ter somado 2x)", err)
		}
	})
	t.Run("não capturada não estorna", func(t *testing.T) {
		a, _ := authorize(c, "pay_r2", 1000, t)
		if err := refund(a, "refund:x", 100); !errors.Is(err, domain.ErrDeclined) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("instável resolve por retry", func(t *testing.T) {
		a := captured("pay_r3", pspsim.AmountRefundFlaky)
		if err := refund(a, "refund:f", 100); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("fora do ar: indeterminado", func(t *testing.T) {
		a := captured("pay_r4", pspsim.AmountRefundDown)
		if err := refund(a, "refund:d", 100); !errors.Is(err, domain.ErrIndeterminate) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("autorização inexistente é erro de contrato, não recusa", func(t *testing.T) {
		err := c.Refund(ctx, domain.RefundRequest{IdempotencyKey: "refund:n", Reference: "auth_nao_existe", Amount: brl(t, 100)})
		if err == nil || errors.Is(err, domain.ErrDeclined) || errors.Is(err, domain.ErrIndeterminate) {
			t.Fatalf("err = %v", err)
		}
	})
	_ = sim
}

// Um redirect não pode transformar o POST de captura num GET "bem-sucedido": o cliente não
// segue redirecionamentos e o 3xx vira erro, sem nunca declarar a captura feita.
func TestClient_DoesNotFollowRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	c := infra.New(fastCfg(redirector.URL))
	err := c.Capture(context.Background(), domain.CaptureRequest{IdempotencyKey: "k", Reference: "auth_1", Amount: brl(t, 100)})
	if err == nil {
		t.Fatal("captura 'bem-sucedida' por causa de um redirect")
	}
	if targetHits.Load() != 0 {
		t.Errorf("o destino do redirect foi chamado %d vezes", targetHits.Load())
	}
}
