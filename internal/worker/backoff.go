package worker

import (
	"math/rand/v2"
	"time"
)

// Backoff calcula a espera antes da próxima tentativa: exponencial, com teto e jitter.
type Backoff struct {
	Base, Max time.Duration
	// Rand devolve [0,1). Injetável para os testes não dependerem de sorte.
	Rand func() float64
}

// Delay para a tentativa que ACABOU de falhar (attempt começa em 1).
// Usa "equal jitter": a espera fica em [teto/2, teto]. Garante um mínimo (não bate no
// endpoint de novo quase na hora) e ainda espalha os retries de eventos que falharam juntos.
func (b Backoff) Delay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	ceil := b.Max
	// Base<<(n) estoura int64 para n grande: só calcula enquanto não passar do teto.
	if shift := attempt - 1; shift < 62 {
		if c := b.Base << shift; c > 0 && c>>shift == b.Base && (c < b.Max || b.Max <= 0) {
			ceil = c
		}
	}
	r := rand.Float64
	if b.Rand != nil {
		r = b.Rand
	}
	half := ceil / 2
	return half + time.Duration(r()*float64(ceil-half))
}
