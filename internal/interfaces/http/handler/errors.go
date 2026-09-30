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
	case errors.Is(err, payment.ErrInvalidTransition):
		writeError(w, http.StatusConflict, "invalid_state", "o pagamento não está em um estado que permita esta operação")
	case errors.Is(err, payment.ErrConcurrentModification):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusConflict, "payment_conflict", "o pagamento foi alterado por outra operação; tente novamente")
	case errors.Is(err, usecase.ErrPSPUnavailable):
		w.Header().Set("Retry-After", "2")
		writeError(w, http.StatusBadGateway, "psp_unavailable",
			"o adquirente não confirmou a operação; repita a requisição com a mesma Idempotency-Key")
	case errors.Is(err, usecase.ErrCaptureRejected):
		writeError(w, http.StatusUnprocessableEntity, "capture_rejected", "o adquirente recusou a captura")
	case errors.Is(err, usecase.ErrVoidRejected):
		writeError(w, http.StatusUnprocessableEntity, "void_rejected", "o adquirente recusou o cancelamento")
	case errors.Is(err, usecase.ErrRefundRejected):
		writeError(w, http.StatusUnprocessableEntity, "refund_rejected", "o adquirente recusou o estorno")
	case errors.Is(err, payment.ErrRefundExceedsCaptured):
		writeError(w, http.StatusUnprocessableEntity, "refund_exceeds_captured", err.Error())
	case errors.Is(err, payment.ErrInvalidAmount):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, payment.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "pagamento não encontrado")
	default:
		slog.ErrorContext(r.Context(), "erro interno", "error", err, "path", r.URL.Path)
		writeError(w, http.StatusInternalServerError, "internal_error", "erro interno")
	}
}
