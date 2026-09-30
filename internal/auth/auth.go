// Package auth define o vocabulário de autenticação compartilhado entre a borda HTTP e a
// infraestrutura, sem que uma importe a outra.
package auth

import "errors"

// ErrUnauthorized: a credencial não existe ou é inválida. É a ÚNICA condição que vira 401.
// Qualquer outro erro do autenticador (banco fora do ar, timeout) é falha nossa, não do
// cliente, e vira 503: responder 401 mascararia a queda e faria o cliente descartar uma
// credencial boa.
var ErrUnauthorized = errors.New("api key inválida")
