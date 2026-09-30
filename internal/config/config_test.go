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

func TestPSPWorstCaseIncludesBackoffs(t *testing.T) {
	// 3 tentativas de 2s + backoffs 1s e 2s (teto 10s) = 6s + 3s = 9s.
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("PSP_MAX_ATTEMPTS", "3")
	t.Setenv("PSP_ATTEMPT_TIMEOUT", "2s")
	t.Setenv("PSP_BASE_BACKOFF", "1s")
	t.Setenv("PSP_MAX_BACKOFF", "10s")
	t.Setenv("IDEMPOTENCY_LEASE", "9s") // exatamente o pior caso: não cobre
	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "IDEMPOTENCY_LEASE") {
		t.Fatalf("err = %v, want erro de IDEMPOTENCY_LEASE", err)
	}
	t.Setenv("IDEMPOTENCY_LEASE", "10s")
	if _, err := config.Load(); err != nil {
		t.Fatalf("lease acima do pior caso deve valer: %v", err)
	}
}

// O corte de reconciliação precisa cobrir também as esperas de backoff, não só os timeouts.
func TestLoadWorker_StaleAfterMustCoverBackoffsToo(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("PSP_MAX_ATTEMPTS", "3")
	t.Setenv("PSP_ATTEMPT_TIMEOUT", "2s") // 6s de timeouts
	t.Setenv("PSP_BASE_BACKOFF", "10s")
	t.Setenv("PSP_MAX_BACKOFF", "10s") // + 20s de backoffs = 26s
	t.Setenv("RECONCILE_STALE_AFTER", "7s")
	if _, err := config.LoadWorker(); err == nil {
		t.Fatal("7s não cobre 26s de pior caso")
	}
	t.Setenv("RECONCILE_STALE_AFTER", "27s")
	if _, err := config.LoadWorker(); err != nil {
		t.Fatalf("27s cobre: %v", err)
	}
}

func TestLoadWorker_LeaseMustCoverAllDeliveryRounds(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("WEBHOOK_BATCH", "100")
	t.Setenv("WEBHOOK_CONCURRENCY", "1") // 100 rodadas
	t.Setenv("WEBHOOK_TIMEOUT", "2s")    // 200s no pior caso
	t.Setenv("WEBHOOK_LEASE", "60s")
	if _, err := config.LoadWorker(); err == nil || !strings.Contains(err.Error(), "WEBHOOK_LEASE") {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("WEBHOOK_CONCURRENCY", "100") // 1 rodada
	if _, err := config.LoadWorker(); err != nil {
		t.Fatalf("1 rodada cabe no lease: %v", err)
	}
}
