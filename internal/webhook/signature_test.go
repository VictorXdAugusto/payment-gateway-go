package webhook_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/webhook"
)

var (
	now  = time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	body = []byte(`{"id":"evt_1","type":"payment.captured"}`)
)

const tol = 5 * time.Minute

func TestSignVerify_RoundTrip(t *testing.T) {
	h := webhook.Sign("whsec_abc", now, body)
	if !strings.HasPrefix(h, fmt.Sprintf("t=%d,v1=", now.Unix())) {
		t.Fatalf("formato inesperado: %s", h)
	}
	if err := webhook.Verify("whsec_abc", h, body, now, tol); err != nil {
		t.Fatalf("assinatura válida rejeitada: %v", err)
	}
}

func TestVerify_Rejections(t *testing.T) {
	valid := webhook.Sign("secret", now, body)
	tests := []struct {
		name    string
		secret  string
		header  string
		body    []byte
		at      time.Time
		wantErr error
	}{
		{"segredo errado", "outro", valid, body, now, webhook.ErrMismatch},
		{"corpo adulterado (1 byte)", "secret", valid, []byte(`{"id":"evt_1","type":"payment.captureD"}`), now, webhook.ErrMismatch},
		{"corpo com espaço a mais", "secret", valid, append([]byte(" "), body...), now, webhook.ErrMismatch},
		{"replay depois da tolerância", "secret", valid, body, now.Add(tol + time.Second), webhook.ErrExpired},
		{"timestamp no futuro", "secret", valid, body, now.Add(-tol - time.Second), webhook.ErrExpired},
		{"header vazio", "secret", "", body, now, webhook.ErrMalformedHeader},
		{"sem v1", "secret", fmt.Sprintf("t=%d", now.Unix()), body, now, webhook.ErrMalformedHeader},
		{"sem timestamp", "secret", "v1=abcd", body, now, webhook.ErrMalformedHeader},
		{"hex inválido", "secret", fmt.Sprintf("t=%d,v1=zzzz", now.Unix()), body, now, webhook.ErrMalformedHeader},
		{"timestamp não numérico", "secret", "t=abc,v1=abcd", body, now, webhook.ErrMalformedHeader},
		{"timestamp duplicado", "secret", fmt.Sprintf("t=%d,t=%d,v1=abcd", now.Unix(), now.Unix()), body, now, webhook.ErrMalformedHeader},
		{"campo sem =", "secret", "lixo", body, now, webhook.ErrMalformedHeader},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := webhook.Verify(tt.secret, tt.header, tt.body, tt.at, tol); !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// O timestamp faz parte do que é assinado: trocar o t= por um recente NÃO reaproveita a assinatura.
func TestVerify_TimestampIsBoundToTheSignature(t *testing.T) {
	old := now.Add(-time.Hour)
	h := webhook.Sign("secret", old, body)
	_, sig, _ := strings.Cut(h, ",")
	forged := fmt.Sprintf("t=%d,%s", now.Unix(), sig)

	if err := webhook.Verify("secret", forged, body, now, tol); !errors.Is(err, webhook.ErrMismatch) {
		t.Fatalf("err = %v: um atacante não pode 'renovar' uma assinatura antiga trocando o t=", err)
	}
}

// Rotação de segredo: durante a troca o emissor manda duas assinaturas v1.
func TestVerify_AcceptsAnyOfSeveralSignatures(t *testing.T) {
	oldSig := strings.TrimPrefix(strings.SplitN(webhook.Sign("old-secret", now, body), ",", 2)[1], "v1=")
	newSig := strings.TrimPrefix(strings.SplitN(webhook.Sign("new-secret", now, body), ",", 2)[1], "v1=")
	h := fmt.Sprintf("t=%d,v1=%s,v1=%s", now.Unix(), oldSig, newSig)

	for _, secret := range []string{"old-secret", "new-secret"} {
		if err := webhook.Verify(secret, h, body, now, tol); err != nil {
			t.Errorf("segredo %q deveria validar: %v", secret, err)
		}
	}
	if err := webhook.Verify("terceiro", h, body, now, tol); !errors.Is(err, webhook.ErrMismatch) {
		t.Errorf("segredo desconhecido: %v", err)
	}
}
