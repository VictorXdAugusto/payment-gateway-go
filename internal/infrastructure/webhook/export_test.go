package webhook

import "time"

// SetDefaultTimeout troca o timeout padrão e devolve a função que o restaura.
func SetDefaultTimeout(d time.Duration) (restore func()) {
	old := defaultTimeout
	defaultTimeout = d
	return func() { defaultTimeout = old }
}
