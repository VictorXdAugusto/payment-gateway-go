package webhook_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	infra "github.com/VictorXdAugusto/payment-gateway-go/internal/infrastructure/webhook"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/outbox"
	sig "github.com/VictorXdAugusto/payment-gateway-go/internal/webhook"
)

func TestIsPublicIP(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"93.184.216.34", true},
		{"2606:4700:4700::1111", true},

		{"127.0.0.1", false},
		{"127.255.255.254", false},
		{"::1", false},
		{"::ffff:127.0.0.1", false}, // loopback disfarçado de IPv6
		{"10.0.0.5", false},
		{"172.16.0.1", false},
		{"172.31.255.255", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false}, // metadados de nuvem (AWS/GCP/Azure)
		{"fe80::1", false},
		{"fc00::1", false},
		{"fd12:3456::1", false},
		{"100.64.0.1", false}, // CGNAT
		{"0.0.0.0", false},
		{"0.1.2.3", false},
		{"::", false},
		{"224.0.0.1", false},
		{"ff02::1", false},
		{"240.0.0.1", false},
		{"198.18.0.1", false},
		{"172.32.0.1", true}, // logo fora da faixa privada 172.16/12
		{"100.128.0.1", true},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			if got := infra.IsPublicIP(netip.MustParseAddr(tt.ip)); got != tt.want {
				t.Errorf("IsPublicIP(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func delivery(url string) outbox.Delivery {
	return outbox.Delivery{
		ID: 1, Attempt: 1, WebhookURL: url, Secret: "whsec_test",
		Event: outbox.Event{EventID: "evt_1", Type: "payment.captured", Payload: []byte(`{"id":"evt_1"}`)},
	}
}

func devSender() *infra.Sender {
	return infra.NewSender(infra.Config{Timeout: 300 * time.Millisecond, AllowPrivate: true})
}

func TestDeliver_Success_SendsSignedRequest(t *testing.T) {
	var gotBody []byte
	var gotHeader http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeader = r.Header.Clone()
		w.WriteHeader(204)
	}))
	defer srv.Close()

	if err := devSender().Deliver(context.Background(), delivery(srv.URL)); err != nil {
		t.Fatal(err)
	}
	if string(gotBody) != `{"id":"evt_1"}` {
		t.Errorf("corpo = %s: os bytes enviados têm que ser exatamente os da outbox", gotBody)
	}
	if gotHeader.Get(sig.EventIDHeader) != "evt_1" || gotHeader.Get("Content-Type") != "application/json" {
		t.Errorf("headers: %v", gotHeader)
	}
	if err := sig.Verify("whsec_test", gotHeader.Get(sig.SignatureHeader), gotBody, time.Now(), time.Minute); err != nil {
		t.Errorf("assinatura enviada não valida: %v", err)
	}
}

func TestDeliver_NonSuccessStatuses_AreRetryableFailures(t *testing.T) {
	for _, code := range []int{400, 401, 404, 408, 429, 500, 502, 503} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte("motivo do erro"))
		}))
		err := devSender().Deliver(context.Background(), delivery(srv.URL))
		srv.Close()

		var se *infra.StatusError
		if !errors.As(err, &se) || se.Code != code || se.Snippet != "motivo do erro" {
			t.Errorf("%d: err = %v", code, err)
		}
		if errors.Is(err, outbox.ErrPermanent) {
			t.Errorf("%d não é permanente: o lojista pode consertar e a próxima tentativa passa", code)
		}
	}
}

// Redirect é a forma clássica de saltar a validação de destino: não seguimos.
func TestDeliver_DoesNotFollowRedirects(t *testing.T) {
	var hit atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Add(1) }))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	err := devSender().Deliver(context.Background(), delivery(redirector.URL))
	var se *infra.StatusError
	if !errors.As(err, &se) || se.Code != http.StatusFound {
		t.Fatalf("err = %v, want StatusError 302", err)
	}
	if hit.Load() != 0 {
		t.Error("o redirect foi seguido")
	}
}

func TestDeliver_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	start := time.Now()
	err := devSender().Deliver(context.Background(), delivery(srv.URL))
	if err == nil || errors.Is(err, outbox.ErrPermanent) {
		t.Fatalf("err = %v, want falha retentável", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("levou %v: o timeout não está valendo", took)
	}
}

// --- SSRF: com a proteção LIGADA (padrão de produção) ---

func TestDeliver_BlocksPrivateDestinations(t *testing.T) {
	var hit atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Add(1) }))
	defer srv.Close() // 127.0.0.1

	prod := infra.NewSender(infra.Config{Timeout: time.Second, AllowPrivate: false})

	tests := map[string]string{
		"loopback (http)":      srv.URL,
		"loopback (https)":     "https://127.0.0.1:9/hook",
		"metadados de nuvem":   "https://169.254.169.254/latest/meta-data/",
		"rede privada":         "https://10.0.0.5/hook",
		"IPv6 loopback":        "https://[::1]:9/hook",
		"IPv4 mapeado em IPv6": "https://[::ffff:127.0.0.1]:9/hook",
		"localhost por nome":   "https://localhost:9/hook",
	}
	for name, url := range tests {
		t.Run(name, func(t *testing.T) {
			err := prod.Deliver(context.Background(), delivery(url))
			if err == nil {
				t.Fatal("entrega para rede interna foi PERMITIDA")
			}
			if !errors.Is(err, outbox.ErrPermanent) {
				t.Errorf("err = %v: destino proibido deve ser falha permanente (repetir não adianta)", err)
			}
		})
	}
	if hit.Load() != 0 {
		t.Errorf("o servidor interno recebeu %d requisição(ões): SSRF", hit.Load())
	}
}

func TestDeliver_RejectsUnsafeURLs(t *testing.T) {
	prod := infra.NewSender(infra.Config{Timeout: time.Second})
	for name, url := range map[string]string{
		"http sem TLS":          "http://example.com/hook",
		"esquema file":          "file:///etc/passwd",
		"esquema gopher":        "gopher://example.com/",
		"credenciais embutidas": "https://user:pass@example.com/hook",
		"sem host":              "https:///hook",
		"lixo":                  "://",
	} {
		t.Run(name, func(t *testing.T) {
			if err := prod.Deliver(context.Background(), delivery(url)); !errors.Is(err, outbox.ErrPermanent) {
				t.Errorf("err = %v, want ErrPermanent", err)
			}
		})
	}
}
