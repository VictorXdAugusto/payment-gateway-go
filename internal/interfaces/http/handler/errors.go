package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/idempotency"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/usecase"
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

// writeDomainError traduz erros da aplicação para HTTP. Erro desconhecido vira 500 SEM
// vazar detalhe interno para o cliente (o detalhe vai para o log).
func writeDomainError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, usecase.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, idempotency.ErrInvalidKey):
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", err.Error())
	case errors.Is(err, idempotency.ErrKeyMismatch):
		writeError(w, http.StatusUnprocessableEntity, "idempotency_key_reuse",
			"esta Idempotency-Key já foi usada com uma requisição diferente")
	case errors.Is(err, idempotency.ErrInFlight), errors.Is(err, idempotency.ErrLockLost):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusConflict, "idempotency_key_in_use",
			"uma requisição com esta Idempotency-Key ainda está em processamento; tente novamente")
	case errors.Is(err, payment.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "pagamento não encontrado")
	default:
		slog.ErrorContext(r.Context(), "erro interno", "error", err, "path", r.URL.Path)
		writeError(w, http.StatusInternalServerError, "internal_error", "erro interno")
	}
}
