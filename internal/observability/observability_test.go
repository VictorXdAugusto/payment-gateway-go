package observability_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/observability"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/psp"
)

// scrape devolve o texto que o Prometheus enxergaria em /metrics.
func scrape(t *testing.T, m *observability.Metrics, db observability.Pinger) string {
	t.Helper()
	srv := httptest.NewServer(observability.OpsHandler(m, db))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

type okDB struct{ err error }

func (d okDB) Ping(context.Context) error { return d.err }

func TestMetrics_HTTPAndWorkerCountersAppearWithTheirLabels(t *testing.T) {
	m := observability.New()
	m.HTTPRequest("POST", "POST /v1/payments", 201, 30*time.Millisecond)
	m.HTTPRequest("POST", "POST /v1/payments", 201, 10*time.Millisecond)
	m.DeliveryOutcome("delivered")
	m.ReconcileResult("resolved")
	m.ReconcileSweep(false)
	m.RetentionDeleted("outbox_events", 7)

	out := scrape(t, m, okDB{})
	for _, want := range []string{
		`gateway_http_requests_total{method="POST",route="POST /v1/payments",status="201"} 2`,
		`gateway_webhook_deliveries_total{outcome="delivered"} 1`,
		`gateway_reconcile_payments_total{result="resolved"} 1`,
		`gateway_reconcile_sweeps_total{aborted="false"} 1`,
		`gateway_retention_deleted_total{policy="outbox_events"} 7`,
		`gateway_http_request_duration_seconds_count{method="POST",route="POST /v1/payments"} 2`,
		`go_goroutines`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("faltou na saída de /metrics: %s", want)
		}
	}
}

type fakeGateway struct{ authErr, captureErr, lookupErr error }

func (f fakeGateway) Authorize(context.Context, psp.AuthorizeRequest) (psp.Authorization, error) {
	return psp.Authorization{Reference: "a1"}, f.authErr
}
func (f fakeGateway) Capture(context.Context, psp.CaptureRequest) error { return f.captureErr }
func (f fakeGateway) Void(context.Context, psp.VoidRequest) error       { return f.captureErr }
func (f fakeGateway) Refund(context.Context, psp.RefundRequest) error   { return f.captureErr }
func (f fakeGateway) Lookup(context.Context, string) (psp.LookupResult, error) {
	return psp.LookupResult{Outcome: psp.OutcomeAuthorized}, f.lookupErr
}

func TestInstrumentedGateway_ClassifiesOutcomesAndKeepsResultsIntact(t *testing.T) {
	m := observability.New()
	amt, _ := money.New(100, money.BRL)
	cases := []struct {
		name    string
		err     error
		outcome string
	}{
		{"ok", nil, "ok"},
		{"declined", &psp.DeclinedError{Code: "x"}, "declined"},
		{"indeterminate", psp.ErrIndeterminate, "indeterminate"},
		{"error", errors.New("bug"), "error"},
	}
	for _, tc := range cases {
		g := observability.InstrumentGateway(fakeGateway{authErr: tc.err}, m)
		out, err := g.Authorize(context.Background(), psp.AuthorizeRequest{IdempotencyKey: "k", Amount: amt})
		if !errors.Is(err, tc.err) || (tc.err == nil && out.Reference != "a1") {
			t.Errorf("%s: o decorator alterou o resultado: %+v / %v", tc.name, out, err)
		}
	}
	text := scrape(t, m, okDB{})
	for _, tc := range cases {
		want := `gateway_psp_requests_total{operation="authorize",outcome="` + tc.outcome + `"} 1`
		if !strings.Contains(text, want) {
			t.Errorf("faltou: %s", want)
		}
	}

	g := observability.InstrumentGateway(fakeGateway{captureErr: psp.ErrIndeterminate}, m)
	_ = g.Capture(context.Background(), psp.CaptureRequest{})
	_, _ = g.Lookup(context.Background(), "k")
	text = scrape(t, m, okDB{})
	for _, want := range []string{
		`gateway_psp_requests_total{operation="capture",outcome="indeterminate"} 1`,
		`gateway_psp_requests_total{operation="lookup",outcome="ok"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("faltou: %s", want)
		}
	}
}

type fakeBacklog struct {
	b   observability.Backlog
	err error
}

func (f fakeBacklog) Backlog(context.Context) (observability.Backlog, error) { return f.b, f.err }

func TestBacklogGauges_ExposeTheSnapshot(t *testing.T) {
	m := observability.New()
	m.RegisterBacklog(fakeBacklog{b: observability.Backlog{
		OutboxPending: 4, OutboxDead: 2, OutboxOldestPending: 90 * time.Second, PaymentsStuck: 3,
	}}, time.Second)
	out := scrape(t, m, okDB{})
	for _, want := range []string{
		"gateway_outbox_pending_events 4", "gateway_outbox_dead_events 2",
		"gateway_outbox_oldest_pending_age_seconds 90", "gateway_payments_stuck 3", "gateway_backlog_scrape_ok 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("faltou: %s\n%s", want, out)
		}
	}
}

// Banco fora: o scrape continua respondendo, sinaliza scrape_ok=0 e NÃO inventa números.
func TestBacklogGauges_DatabaseFailureReportsScrapeNotOK(t *testing.T) {
	m := observability.New()
	m.RegisterBacklog(fakeBacklog{err: errors.New("banco fora")}, time.Second)
	out := scrape(t, m, okDB{})
	if !strings.Contains(out, "gateway_backlog_scrape_ok 0") || strings.Contains(out, "gateway_payments_stuck") {
		t.Errorf("saída inesperada:\n%s", out)
	}
}

func TestOpsHandler_HealthAndReadiness(t *testing.T) {
	m := observability.New()
	for name, tc := range map[string]struct {
		path string
		db   observability.Pinger
		want int
	}{
		"health ignora o banco": {"/health", okDB{err: errors.New("fora")}, 200},
		"ready com banco ok":    {"/ready", okDB{}, 200},
		"ready com banco fora":  {"/ready", okDB{err: errors.New("fora")}, 503},
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			observability.OpsHandler(m, tc.db).ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestMetrics_CounterValueViaTestutil(t *testing.T) {
	// Garante que a dependência testutil está disponível e que o registro não duplica nomes.
	m := observability.New()
	if n, err := testutil.GatherAndCount(m.Registry, "gateway_http_requests_total"); err != nil || n != 0 {
		t.Errorf("n=%d err=%v", n, err)
	}
}
