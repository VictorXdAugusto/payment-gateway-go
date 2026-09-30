package webhooksink_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/webhook"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/webhooksink"
)

func post(t *testing.T, srv *httptest.Server, secret, body string, at time.Time) int {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/hook", strings.NewReader(body))
	req.Header.Set(webhook.SignatureHeader, webhook.Sign(secret, at, []byte(body)))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func received(t *testing.T, srv *httptest.Server) []webhooksink.Received {
	t.Helper()
	res, err := http.Get(srv.URL + "/received")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out []webhooksink.Received
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSink_VerifiesSignatureAndDeduplicates(t *testing.T) {
	srv := httptest.NewServer(webhooksink.New("whsec", 0).Handler())
	defer srv.Close()
	body := `{"id":"evt_1","type":"payment.captured"}`

	if got := post(t, srv, "whsec", body, time.Now()); got != 200 {
		t.Fatalf("status = %d", got)
	}
	if got := post(t, srv, "whsec", body, time.Now()); got != 200 { // reentrega do mesmo evento
		t.Fatalf("reentrega: status = %d", got)
	}
	evs := received(t, srv)
	if len(evs) != 1 || evs[0].EventID != "evt_1" || evs[0].Attempts != 2 {
		t.Errorf("recebidos = %+v, want 1 evento visto 2 vezes", evs)
	}
}

func TestSink_RejectsBadSignaturesAndReplays(t *testing.T) {
	srv := httptest.NewServer(webhooksink.New("whsec", 0).Handler())
	defer srv.Close()
	body := `{"id":"evt_1","type":"x"}`

	if got := post(t, srv, "segredo-errado", body, time.Now()); got != 401 {
		t.Errorf("assinatura errada: status = %d", got)
	}
	if got := post(t, srv, "whsec", body, time.Now().Add(-time.Hour)); got != 401 {
		t.Errorf("replay antigo: status = %d", got)
	}
	if len(received(t, srv)) != 0 {
		t.Error("nada deve ser processado sem assinatura válida")
	}
}

func TestSink_FailsTheFirstAttemptsOfEachEvent(t *testing.T) {
	srv := httptest.NewServer(webhooksink.New("whsec", 2).Handler())
	defer srv.Close()
	a, b := `{"id":"evt_a","type":"x"}`, `{"id":"evt_b","type":"x"}`

	codes := []int{post(t, srv, "whsec", a, time.Now()), post(t, srv, "whsec", a, time.Now()), post(t, srv, "whsec", a, time.Now())}
	if codes[0] != 500 || codes[1] != 500 || codes[2] != 200 {
		t.Errorf("evt_a: %v, want [500 500 200]", codes)
	}
	if got := post(t, srv, "whsec", b, time.Now()); got != 500 { // a contagem é POR evento
		t.Errorf("evt_b primeira tentativa: %d, want 500", got)
	}
}
