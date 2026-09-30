package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
)

// StuckLister enumera pagamentos presos. Implementado pelo repositório de pagamentos.
type StuckLister interface {
	ListStuck(ctx context.Context, before time.Time, afterID payment.ID, limit int, leaseWindow time.Duration) ([]payment.ID, error)
}

// Resolver tenta resolver UM pagamento preso e devolve o status depois da tentativa.
type Resolver interface {
	Execute(ctx context.Context, id payment.ID) (payment.Status, error)
}

type ReconcilerConfig struct {
	Interval   time.Duration // pausa entre varreduras
	StaleAfter time.Duration // só mexe em pagamento parado há mais que isso (não atropela requisição viva)
	// IdempotencyLease: pagamentos cuja chave de idempotência está travada há menos que isso têm
	// uma requisição viva e são pulados. Deve ser o mesmo IDEMPOTENCY_LEASE do servidor.
	IdempotencyLease time.Duration
	PageSize         int // quantos ids por consulta
	// MaxConsecutiveErrors interrompe a varredura quando o PSP parece fora do ar: insistir só
	// gera carga num serviço doente. A próxima varredura tenta de novo.
	MaxConsecutiveErrors int

	// Hooks opcionais (métricas). OnResult: "resolved", "pending" ou "error", por pagamento.
	OnResult func(result string)
	// OnSweep: uma vez por varredura concluída (aborted = PSP parecia fora do ar).
	OnSweep func(aborted bool)
}

// Report resume uma varredura.
type Report struct {
	Examined int // pagamentos presos que tentamos resolver
	Resolved int // saíram do estado preso (autorizado ou falho)
	Pending  int // continuam presos (PSP ainda não sabe, dentro da carência)
	Errors   int // falharam ao tentar (PSP fora do ar, banco)
	Aborted  bool
}

// Reconciler varre periodicamente os pagamentos presos e pede ao caso de uso que os resolva.
// É o que fecha o ciclo do estado unknown: sem ele, um timeout no PSP viraria pendência eterna.
type Reconciler struct {
	mu       sync.Mutex
	resume   payment.ID // onde a próxima varredura recomeça depois de uma varredura abortada
	payments StuckLister
	resolve  Resolver
	cfg      ReconcilerConfig
	log      *slog.Logger
	now      func() time.Time
}

func NewReconciler(payments StuckLister, resolve Resolver, cfg ReconcilerConfig, log *slog.Logger, now func() time.Time) *Reconciler {
	if cfg.PageSize < 1 {
		cfg.PageSize = 1
	}
	if cfg.MaxConsecutiveErrors < 1 {
		cfg.MaxConsecutiveErrors = 1
	}
	return &Reconciler{payments: payments, resolve: resolve, cfg: cfg, log: log, now: now}
}

// Run repete RunOnce até ctx ser cancelado.
func (r *Reconciler) Run(ctx context.Context) {
	for ctx.Err() == nil {
		rep, err := r.RunOnce(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			r.log.Error("varredura de reconciliação falhou", "error", err)
		case rep.Examined > 0 || rep.Aborted:
			r.log.Info("varredura de reconciliação concluída",
				"examined", rep.Examined, "resolved", rep.Resolved, "pending", rep.Pending,
				"errors", rep.Errors, "aborted", rep.Aborted)
		}
		select {
		case <-ctx.Done():
		case <-time.After(r.cfg.Interval):
		}
	}
}

// RunOnce percorre TODOS os pagamentos presos elegíveis, página a página, com cursor por id.
// O cursor evita a fome: pagamentos que continuam presos não bloqueiam os que vêm depois.
func (r *Reconciler) RunOnce(ctx context.Context) (Report, error) {
	rep, err := r.sweep(ctx)
	if err == nil && ctx.Err() == nil && r.cfg.OnSweep != nil {
		r.cfg.OnSweep(rep.Aborted)
	}
	return rep, err
}

func (r *Reconciler) result(res string) {
	if r.cfg.OnResult != nil {
		r.cfg.OnResult(res)
	}
}

func (r *Reconciler) sweep(ctx context.Context) (Report, error) {
	// Uma varredura por vez. O ponto de retomada evita a fome: se os primeiros pagamentos da fila
	// falham SEMPRE (dado corrompido, por exemplo) e abortam a varredura, a próxima recomeça depois
	// deles em vez de bater nos mesmos de novo e nunca chegar aos demais.
	r.mu.Lock()
	defer r.mu.Unlock()
	var (
		rep         Report
		after       = r.resume
		consecutive int
		cutoff      = r.now().Add(-r.cfg.StaleAfter)
	)
	r.resume = ""
	for ctx.Err() == nil {
		ids, err := r.payments.ListStuck(ctx, cutoff, after, r.cfg.PageSize, r.cfg.IdempotencyLease)
		if err != nil {
			return rep, err
		}
		for _, id := range ids {
			if ctx.Err() != nil {
				return rep, nil
			}
			rep.Examined++
			status, err := r.resolve.Execute(ctx, id)
			switch {
			case err != nil && ctx.Err() == nil:
				rep.Errors++
				consecutive++
				r.result("error")
				r.log.Warn("não foi possível reconciliar o pagamento", "payment_id", id, "error", err)
				if consecutive >= r.cfg.MaxConsecutiveErrors {
					rep.Aborted = true
					r.resume = id // a próxima varredura continua DEPOIS deste
					return rep, nil
				}
			case err != nil:
				return rep, nil // desligando
			case status == payment.StatusCreated || status == payment.StatusUnknown:
				consecutive = 0
				rep.Pending++
				r.result("pending")
			default:
				consecutive = 0
				rep.Resolved++
				r.result("resolved")
				r.log.Info("pagamento reconciliado", "payment_id", id, "status", status)
			}
		}
		if len(ids) < r.cfg.PageSize {
			return rep, nil
		}
		after = ids[len(ids)-1]
	}
	return rep, nil // ctx cancelado: desligando
}
