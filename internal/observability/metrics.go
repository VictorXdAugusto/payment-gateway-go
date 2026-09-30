// Package observability expõe as métricas Prometheus do gateway e o servidor de operação
// (/metrics, /health, /ready), que roda numa porta separada da API pública.
package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Rótulos com valores de um conjunto FECHADO. Nunca use id de pagamento, lojista ou caminho
// cru como rótulo: a cardinalidade explodiria a memória do Prometheus.
const (
	namespace = "gateway"
)

// Metrics agrupa todos os instrumentos. Cada processo (servidor, worker) cria o seu e
// registra só o que usa, mas os nomes são os mesmos para os dashboards serem únicos.
type Metrics struct {
	Registry *prometheus.Registry

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec

	pspRequests *prometheus.CounterVec
	pspDuration *prometheus.HistogramVec

	deliveries       *prometheus.CounterVec
	reconcileResults *prometheus.CounterVec
	reconcileSweeps  *prometheus.CounterVec
	retentionDeleted *prometheus.CounterVec
}

// New cria o registro com as métricas de runtime do Go e do processo já incluídas.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	m := &Metrics{
		Registry: reg,
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "http_requests_total",
			Help: "Requisições HTTP atendidas, por método, rota (padrão do mux) e status.",
		}, []string{"method", "route", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "http_request_duration_seconds",
			Help:    "Latência das requisições HTTP.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"method", "route"}),
		pspRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "psp_requests_total",
			Help: "Chamadas ao PSP, por operação e desfecho (ok, declined, indeterminate, error).",
		}, []string{"operation", "outcome"}),
		pspDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "psp_request_duration_seconds",
			Help:    "Latência das chamadas ao PSP (inclui os retries do cliente).",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2, 5, 10},
		}, []string{"operation"}),
		deliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "webhook_deliveries_total",
			Help: "Resultados de entrega de webhook: delivered, rescheduled, dead, skipped, claim_lost.",
		}, []string{"outcome"}),
		reconcileResults: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "reconcile_payments_total",
			Help: "Pagamentos presos examinados pelo reconciliador: resolved, pending, error.",
		}, []string{"result"}),
		reconcileSweeps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "reconcile_sweeps_total",
			Help: "Varreduras do reconciliador, por terem sido abortadas (PSP fora do ar) ou não.",
		}, []string{"aborted"}),
		retentionDeleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "retention_deleted_total",
			Help: "Registros apagados pela política de retenção.",
		}, []string{"policy"}),
	}
	reg.MustRegister(m.httpRequests, m.httpDuration, m.pspRequests, m.pspDuration,
		m.deliveries, m.reconcileResults, m.reconcileSweeps, m.retentionDeleted)
	return m
}
