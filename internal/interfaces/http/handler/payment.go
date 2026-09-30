package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/interfaces/http/middleware"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/usecase"
)

const maxBodyBytes = 1 << 20 // 1 MiB

type CreatePaymentUseCase interface {
	Execute(ctx context.Context, in usecase.CreatePaymentInput) (usecase.CreatePaymentOutput, error)
}

type CapturePaymentUseCase interface {
	Execute(ctx context.Context, in usecase.CapturePaymentInput) (usecase.CreatePaymentOutput, error)
}

type GetPaymentUseCase interface {
	Execute(ctx context.Context, merchantID, id string) (usecase.PaymentView, error)
}

type Payment struct {
	create  CreatePaymentUseCase
	capture CapturePaymentUseCase
	get     GetPaymentUseCase
}

func NewPayment(create CreatePaymentUseCase, capture CapturePaymentUseCase, get GetPaymentUseCase) *Payment {
	return &Payment{create: create, capture: capture, get: get}
}

type createPaymentRequest struct {
	Amount   *int64 `json:"amount"` // ponteiro: distingue "ausente" de 0
	Currency string `json:"currency"`
}

// Create: POST /v1/payments  (header Idempotency-Key obrigatório)
func (h *Payment) Create(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "missing_idempotency_key", "o header Idempotency-Key é obrigatório")
		return
	}

	var req createPaymentRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Amount == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "amount é obrigatório")
		return
	}

	out, err := h.create.Execute(r.Context(), usecase.CreatePaymentInput{
		MerchantID:     middleware.MerchantID(r.Context()),
		IdempotencyKey: key,
		Amount:         *req.Amount,
		Currency:       req.Currency,
	})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	if out.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, http.StatusCreated, out.Payment)
}

// Capture: POST /v1/payments/{id}/capture  (header Idempotency-Key obrigatório, sem corpo)
func (h *Payment) Capture(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "missing_idempotency_key", "o header Idempotency-Key é obrigatório")
		return
	}

	out, err := h.capture.Execute(r.Context(), usecase.CapturePaymentInput{
		MerchantID:     middleware.MerchantID(r.Context()),
		PaymentID:      r.PathValue("id"),
		IdempotencyKey: key,
	})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	if out.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, http.StatusOK, out.Payment)
}

// Get: GET /v1/payments/{id}
func (h *Payment) Get(w http.ResponseWriter, r *http.Request) {
	view, err := h.get.Execute(r.Context(), middleware.MerchantID(r.Context()), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// decodeJSON é estrito de propósito: limita o tamanho, rejeita campo desconhecido
// (typo silencioso vira erro) e rejeita lixo depois do objeto.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("corpo JSON inválido: " + err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("corpo JSON inválido: conteúdo após o objeto")
	}
	return nil
}
