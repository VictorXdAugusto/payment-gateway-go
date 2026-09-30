package worker

import (
	"context"
	"log/slog"
	"time"
)

// Purger apaga um lote de registros antigos e devolve quantos apagou.
type Purger interface {
	Purge(ctx context.Context, olderThan time.Duration, batch int) (int64, error)
}

// PurgeFunc adapta uma função a Purger.
type PurgeFunc func(ctx context.Context, olderThan time.Duration, batch int) (int64, error)

func (f PurgeFunc) Purge(ctx context.Context, olderThan time.Duration, batch int) (int64, error) {
	return f(ctx, olderThan, batch)
}

// Retention descreve UMA política de retenção.
type Retention struct {
	Name      string
	OlderThan time.Duration
	Purger    Purger
}

type HousekeeperConfig struct {
	Interval time.Duration
	Batch    int // tamanho de cada DELETE: lotes pequenos não seguram lock por muito tempo

	// OnPurged (opcional) recebe quantos registros cada lote apagou (métricas).
	OnPurged func(policy string, n int64)
}

// Housekeeper aplica as políticas de retenção (chaves de idempotência, eventos entregues).
// Sem ele as duas tabelas crescem sem limite.
type Housekeeper struct {
	policies []Retention
	cfg      HousekeeperConfig
	log      *slog.Logger
}

func NewHousekeeper(policies []Retention, cfg HousekeeperConfig, log *slog.Logger) *Housekeeper {
	if cfg.Batch < 1 {
		cfg.Batch = 1
	}
	return &Housekeeper{policies: policies, cfg: cfg, log: log}
}

func (h *Housekeeper) Run(ctx context.Context) {
	for ctx.Err() == nil {
		h.RunOnce(ctx)
		select {
		case <-ctx.Done():
		case <-time.After(h.cfg.Interval):
		}
	}
}

// RunOnce esgota cada política em lotes. Um erro numa política não impede as outras.
func (h *Housekeeper) RunOnce(ctx context.Context) {
	for _, p := range h.policies {
		var total int64
		for ctx.Err() == nil {
			n, err := p.Purger.Purge(ctx, p.OlderThan, h.cfg.Batch)
			if err != nil {
				if ctx.Err() == nil {
					h.log.Error("limpeza de retenção falhou", "policy", p.Name, "error", err)
				}
				break
			}
			total += n
			if n > 0 && h.cfg.OnPurged != nil {
				h.cfg.OnPurged(p.Name, n)
			}
			if n < int64(h.cfg.Batch) {
				break
			}
		}
		if total > 0 {
			h.log.Info("retenção aplicada", "policy", p.Name, "deleted", total)
		}
	}
}
