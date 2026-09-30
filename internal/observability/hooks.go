package observability

import (
	"strconv"
	"time"
)

// HTTPRequest registra uma requisição atendida. route é o padrão do mux ("POST /v1/payments"),
// nunca o caminho cru.
func (m *Metrics) HTTPRequest(method, route string, status int, d time.Duration) {
	m.httpRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.httpDuration.WithLabelValues(method, route).Observe(d.Seconds())
}

// PSPCall registra uma chamada ao PSP.
func (m *Metrics) PSPCall(operation, outcome string, d time.Duration) {
	m.pspRequests.WithLabelValues(operation, outcome).Inc()
	m.pspDuration.WithLabelValues(operation).Observe(d.Seconds())
}

// DeliveryOutcome registra o resultado de uma entrega de webhook.
func (m *Metrics) DeliveryOutcome(outcome string) { m.deliveries.WithLabelValues(outcome).Inc() }

// ReconcileResult registra o resultado de UM pagamento examinado.
func (m *Metrics) ReconcileResult(result string) { m.reconcileResults.WithLabelValues(result).Inc() }

// ReconcileSweep registra o fim de uma varredura.
func (m *Metrics) ReconcileSweep(aborted bool) {
	m.reconcileSweeps.WithLabelValues(strconv.FormatBool(aborted)).Inc()
}

// RetentionDeleted registra quantos registros uma política apagou.
func (m *Metrics) RetentionDeleted(policy string, n int64) {
	m.retentionDeleted.WithLabelValues(policy).Add(float64(n))
}
