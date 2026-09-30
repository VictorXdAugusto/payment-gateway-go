package idempotency_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/idempotency"
)

func TestValidateKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"uuid", "3f2b8c1e-9d4a-4c55-8a3e-0e6c1f7d2b90", false},
		{"curta", "a", false},
		{"255 caracteres", strings.Repeat("k", 255), false},
		{"com espaço no meio", "pedido 123", false},
		{"unicode imprimível", "pedido-ção", false},
		{"vazia", "", true},
		{"256 caracteres", strings.Repeat("k", 256), true},
		{"espaço no início", " abc", true},
		{"espaço no fim", "abc ", true},
		{"quebra de linha", "abc\ndef", true},
		{"caractere de controle", "abc\x00def", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := idempotency.ValidateKey(tt.key)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, idempotency.ErrInvalidKey) {
				t.Errorf("err = %v, want ErrInvalidKey", err)
			}
		})
	}
}

func TestFingerprint(t *testing.T) {
	a := idempotency.Fingerprint("create_payment", "1000", "BRL")

	if a != idempotency.Fingerprint("create_payment", "1000", "BRL") {
		t.Error("o hash precisa ser determinístico")
	}
	if a == idempotency.Fingerprint("create_payment", "1001", "BRL") {
		t.Error("valor diferente precisa gerar hash diferente")
	}
	if idempotency.Fingerprint("ab", "c") == idempotency.Fingerprint("a", "bc") {
		t.Error("as fronteiras entre partes precisam contar: (ab,c) != (a,bc)")
	}
	if idempotency.Fingerprint("a", "") == idempotency.Fingerprint("a") {
		t.Error("parte vazia precisa contar")
	}
}
