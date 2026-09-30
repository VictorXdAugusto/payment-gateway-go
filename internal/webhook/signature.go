// Package webhook implementa a assinatura das entregas. O esquema é o da Stripe:
//
//	X-Gateway-Signature: t=1700000000,v1=<hex(HMAC-SHA256(secret, "<t>.<corpo>"))>
//
// O timestamp entra no que é assinado, então uma entrega interceptada não pode ser
// reenviada mais tarde por um atacante (o receptor rejeita timestamps fora da tolerância).
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	SignatureHeader = "X-Gateway-Signature"
	EventIDHeader   = "X-Gateway-Event-Id"
)

var (
	ErrMalformedHeader = errors.New("header de assinatura malformado")
	ErrExpired         = errors.New("timestamp fora da tolerância (possível replay)")
	ErrMismatch        = errors.New("assinatura não confere")
)

func mac(secret string, ts int64, body []byte) []byte {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(strconv.FormatInt(ts, 10)))
	h.Write([]byte("."))
	h.Write(body)
	return h.Sum(nil)
}

// Sign gera o valor do header para o instante now.
func Sign(secret string, now time.Time, body []byte) string {
	ts := now.Unix()
	return "t=" + strconv.FormatInt(ts, 10) + ",v1=" + hex.EncodeToString(mac(secret, ts, body))
}

// Verify confere o header. Aceita VÁRIAS assinaturas v1 (rotação de segredo: durante a troca
// o emissor assina com o segredo velho e o novo) e compara em tempo constante.
func Verify(secret, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var (
		ts     int64
		hasTS  bool
		sigs   [][]byte
		fields = strings.Split(header, ",")
	)
	for _, f := range fields {
		k, v, ok := strings.Cut(strings.TrimSpace(f), "=")
		if !ok {
			return ErrMalformedHeader
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || hasTS {
				return ErrMalformedHeader
			}
			ts, hasTS = n, true
		case "v1":
			b, err := hex.DecodeString(v)
			if err != nil {
				return ErrMalformedHeader
			}
			sigs = append(sigs, b)
		}
	}
	if !hasTS || len(sigs) == 0 {
		return ErrMalformedHeader
	}

	if d := now.Sub(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
		return ErrExpired
	}

	want := mac(secret, ts, body)
	for _, s := range sigs {
		if hmac.Equal(s, want) {
			return nil
		}
	}
	return ErrMismatch
}
