package observability

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Backlog é o retrato do que está esperando para ser processado.
type Backlog struct {
	OutboxPending       int64
	OutboxDead          int64
	OutboxOldestPending time.Duration // quanto tempo a entrega mais atrasada está vencida (0 = nenhuma)
	PaymentsStuck       int64         // created ou unknown, ainda não reconciliados
}

// BacklogSource lê o backlog (implementado sobre o Postgres).
type BacklogSource interface {
	Backlog(ctx context.Context) (Backlog, error)
}

// backlogCollector consulta a fonte A CADA SCRAPE, com timeout curto: se o banco estiver
// lento, o scrape não trava (as séries somem naquele ciclo e gateway_backlog_scrape_ok vira 0).
type backlogCollector struct {
	src     BacklogSource
	timeout time.Duration

	pending, dead, oldest, stuck, ok *prometheus.Desc
}

func newBacklogCollector(src BacklogSource, timeout time.Duration) *backlogCollector {
	d := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc(namespace+"_"+name, help, nil, nil)
	}
	return &backlogCollector{
		src: src, timeout: timeout,
		pending: d("outbox_pending_events", "Eventos da outbox aguardando entrega."),
		dead:    d("outbox_dead_events", "Eventos que esgotaram as tentativas (precisam de atenção humana)."),
		oldest:  d("outbox_oldest_pending_age_seconds", "Atraso da entrega pendente mais antiga (0 = em dia)."),
		stuck:   d("payments_stuck", "Pagamentos em created/unknown que a reconciliação ainda não resolveu."),
		ok:      d("backlog_scrape_ok", "1 se a consulta de backlog deste scrape funcionou."),
	}
}

func (c *backlogCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.pending, c.dead, c.oldest, c.stuck, c.ok} {
		ch <- d
	}
}

func (c *backlogCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	b, err := c.src.Backlog(ctx)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.ok, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.ok, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(c.pending, prometheus.GaugeValue, float64(b.OutboxPending))
	ch <- prometheus.MustNewConstMetric(c.dead, prometheus.GaugeValue, float64(b.OutboxDead))
	ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, b.OutboxOldestPending.Seconds())
	ch <- prometheus.MustNewConstMetric(c.stuck, prometheus.GaugeValue, float64(b.PaymentsStuck))
}

// RegisterBacklog liga o coletor de backlog ao registro.
func (m *Metrics) RegisterBacklog(src BacklogSource, timeout time.Duration) {
	m.Registry.MustRegister(newBacklogCollector(src, timeout))
}
