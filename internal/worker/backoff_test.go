package worker_test

import (
	"testing"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/worker"
)

func TestBackoff_CeilingDoublesAndIsCapped(t *testing.T) {
	// Rand=1 devolve o teto da faixa; Rand=0 devolve o piso (teto/2).
	hi := worker.Backoff{Base: 10 * time.Second, Max: time.Hour, Rand: func() float64 { return 0.999999999 }}
	lo := worker.Backoff{Base: 10 * time.Second, Max: time.Hour, Rand: func() float64 { return 0 }}

	ceilings := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second}
	for i, want := range ceilings {
		attempt := i + 1
		if got := lo.Delay(attempt); got != want/2 {
			t.Errorf("tentativa %d, piso = %v, want %v", attempt, got, want/2)
		}
		if got := hi.Delay(attempt); got < want-time.Millisecond || got > want {
			t.Errorf("tentativa %d, teto = %v, want ~%v", attempt, got, want)
		}
	}
}

func TestBackoff_NeverExceedsMax_EvenForAbsurdAttempts(t *testing.T) {
	b := worker.Backoff{Base: 10 * time.Second, Max: time.Hour, Rand: func() float64 { return 0.999999999 }}
	for _, attempt := range []int{12, 30, 61, 62, 63, 64, 100, 1 << 20, -5, 0} {
		got := b.Delay(attempt)
		if got <= 0 || got > time.Hour {
			t.Errorf("tentativa %d: delay = %v, deve estar em (0, 1h]", attempt, got)
		}
	}
}

func TestBackoff_JitterStaysInsideTheBand(t *testing.T) {
	b := worker.Backoff{Base: time.Second, Max: time.Minute} // Rand real
	for i := 0; i < 2000; i++ {
		got := b.Delay(4) // teto 8s -> faixa [4s, 8s]
		if got < 4*time.Second || got > 8*time.Second {
			t.Fatalf("delay = %v fora de [4s, 8s]", got)
		}
	}
}
