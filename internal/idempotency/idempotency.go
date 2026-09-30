// Package idempotency garante que uma operação disparada N vezes com a mesma chave
// produz UM efeito. Modelo inspirado no da Stripe: a chave carrega um "recovery point"
// (até onde já chegamos) para que um retry retome de onde parou em vez de recomeçar.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"unicode"
)

const (
	// PointStarted: a chave foi registrada e nenhum efeito aconteceu ainda.
	PointStarted = "started"
	// PointFinished: a operação terminou e a resposta está guardada para replay.
	PointFinished = "finished"
)

const maxKeyLength = 255

var (
	ErrInvalidKey = errors.New("chave de idempotência inválida")
	// ErrKeyMismatch: a mesma chave chegou com um corpo diferente (uso errado pelo cliente).
	ErrKeyMismatch = errors.New("chave de idempotência já usada com outra requisição")
	// ErrInFlight: outra requisição com esta chave está sendo processada agora.
	ErrInFlight = errors.New("requisição com esta chave ainda em processamento")
	// ErrLockLost: o lease expirou e outro processo assumiu a chave (o token não vale mais).
	ErrLockLost = errors.New("lock da chave de idempotência perdido")
)

// Key identifica uma operação: as chaves são isoladas por lojista.
type Key struct {
	MerchantID string
	Value      string
}

// ValidateKey aceita 1 a 255 caracteres imprimíveis, sem espaços nas pontas.
func ValidateKey(v string) error {
	if v == "" || len(v) > maxKeyLength {
		return fmt.Errorf("%w: deve ter entre 1 e %d caracteres", ErrInvalidKey, maxKeyLength)
	}
	for _, r := range v {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("%w: caractere não imprimível", ErrInvalidKey)
		}
	}
	if v[0] == ' ' || v[len(v)-1] == ' ' {
		return fmt.Errorf("%w: espaços nas pontas", ErrInvalidKey)
	}
	return nil
}

// Fingerprint resume a requisição num hash estável. Cada parte entra com o tamanho na
// frente, então ("ab","c") e ("a","bc") nunca colidem.
func Fingerprint(parts ...string) string {
	h := sha256.New()
	var n [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		h.Write(n[:])
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Acquisition é o resultado de Acquire.
type Acquisition struct {
	Key           Key
	Token         int64  // fencing token: vale para Advance/Finish/Release desta aquisição
	RecoveryPoint string // onde retomar (PointStarted na primeira vez)
	ResourceID    string // id do recurso criado numa fase anterior, se houver
	Replay        []byte // != nil: a operação já terminou; devolva esta resposta e pare
}

// Store é a porta de persistência das chaves.
type Store interface {
	// Acquire registra a chave (primeira vez) ou assume o processamento (retry).
	// Erros: ErrKeyMismatch, ErrInFlight. Deve ser chamado FORA de transação.
	Acquire(ctx context.Context, key Key, requestHash string) (Acquisition, error)

	// Advance move o recovery point e guarda o id do recurso. Participa da transação do
	// contexto, então avança atomicamente junto com os efeitos da fase. ErrLockLost se
	// o token não for mais o vigente.
	Advance(ctx context.Context, a Acquisition, point, resourceID string) error

	// Finish guarda a resposta, marca como terminada e libera o lock. Também participa da
	// transação do contexto. ErrLockLost se o token não for mais o vigente.
	Finish(ctx context.Context, a Acquisition, response []byte) error

	// Release solta o lock sem terminar, para o retry do cliente não esperar o lease.
	// Mantém o recovery point. Nunca falha por lock perdido.
	Release(ctx context.Context, a Acquisition) error
}
