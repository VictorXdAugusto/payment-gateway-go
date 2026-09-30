package config_test

import (
	"strings"
	"testing"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/config"
)

func TestLoadWorker_Defaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	c, err := config.LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if c.ReconcileStaleAfter <= 0 || c.UnknownGrace <= 0 || c.IdempotencyRetention.Hours() != 24 || c.OutboxRetention.Hours() != 168 {
		t.Errorf("defaults inesperados: %+v", c)
	}
}

func TestLoadWorker_RequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if _, err := config.LoadWorker(); err == nil {
		t.Fatal("DATABASE_URL vazia deve falhar")
	}
}

// Reconciliar antes de o pior caso de uma requisição viva terminar atropelaria essa requisição.
func TestLoadWorker_RejectsStaleAfterShorterThanWorstCasePSPCall(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("PSP_MAX_ATTEMPTS", "5")
	t.Setenv("PSP_ATTEMPT_TIMEOUT", "10s") // pior caso: 50s
	t.Setenv("RECONCILE_STALE_AFTER", "30s")
	_, err := config.LoadWorker()
	if err == nil || !strings.Contains(err.Error(), "RECONCILE_STALE_AFTER") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadWorker_RejectsLeaseNotLongerThanDeliveryTimeout(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("WEBHOOK_LEASE", "5s")
	t.Setenv("WEBHOOK_TIMEOUT", "5s")
	if _, err := config.LoadWorker(); err == nil {
		t.Fatal("lease <= timeout deve falhar")
	}
}

func TestLoadWorker_RejectsInvalidValues(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"RECONCILE_INTERVAL", "abc"}, {"RECONCILE_INTERVAL", "0s"}, {"RECONCILE_PAGE_SIZE", "0"},
		{"RETENTION_BATCH", "x"}, {"OUTBOX_RETENTION", "-1h"},
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://x")
			t.Setenv(tc.key, tc.val)
			if _, err := config.LoadWorker(); err == nil {
				t.Fatal("esperava erro")
			}
		})
	}
}
