package worker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/worker"
)

// scripted devolve, a cada chamada, o próximo número de registros apagados.
func scripted(counts ...int64) (worker.PurgeFunc, *[]int64) {
	var calls []int64
	return func(_ context.Context, _ time.Duration, batch int) (int64, error) {
		calls = append(calls, int64(batch))
		if len(counts) == 0 {
			return 0, nil
		}
		n := counts[0]
		counts = counts[1:]
		return n, nil
	}, &calls
}

// Lote cheio significa "ainda há mais": repete até vir um lote incompleto.
func TestHousekeeper_DrainsInBatches(t *testing.T) {
	purge, calls := scripted(10, 10, 4)
	h := worker.NewHousekeeper([]worker.Retention{{Name: "x", OlderThan: time.Hour, Purger: purge}},
		worker.HousekeeperConfig{Batch: 10}, quiet())
	h.RunOnce(context.Background())
	if len(*calls) != 3 {
		t.Errorf("chamadas = %d, want 3 (10, 10, 4)", len(*calls))
	}
}

func TestHousekeeper_AFailingPolicyDoesNotBlockTheOthers(t *testing.T) {
	bad := worker.PurgeFunc(func(context.Context, time.Duration, int) (int64, error) { return 0, errors.New("boom") })
	good, calls := scripted(1)
	h := worker.NewHousekeeper([]worker.Retention{
		{Name: "bad", OlderThan: time.Hour, Purger: bad},
		{Name: "good", OlderThan: time.Hour, Purger: good},
	}, worker.HousekeeperConfig{Batch: 10}, quiet())
	h.RunOnce(context.Background())
	if len(*calls) != 1 {
		t.Errorf("a política boa não rodou: %d chamadas", len(*calls))
	}
}

func TestHousekeeper_PassesTheRetentionAge(t *testing.T) {
	var got time.Duration
	p := worker.PurgeFunc(func(_ context.Context, age time.Duration, _ int) (int64, error) { got = age; return 0, nil })
	worker.NewHousekeeper([]worker.Retention{{Name: "x", OlderThan: 36 * time.Hour, Purger: p}},
		worker.HousekeeperConfig{Batch: 5}, quiet()).RunOnce(context.Background())
	if got != 36*time.Hour {
		t.Errorf("idade = %v", got)
	}
}
