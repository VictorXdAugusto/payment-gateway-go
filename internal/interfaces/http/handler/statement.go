package handler

import (
	"context"
	"net/http"
	"strconv"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/interfaces/http/middleware"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/usecase"
)

type GetBalanceUseCase interface {
	Execute(ctx context.Context, merchantID string) ([]usecase.BalanceView, error)
}

type GetStatementUseCase interface {
	Execute(ctx context.Context, merchantID string, before int64, limit int) (usecase.StatementPage, error)
}

// Account expõe o saldo e o extrato do lojista autenticado.
type Account struct {
	balance   GetBalanceUseCase
	statement GetStatementUseCase
}

func NewAccount(balance GetBalanceUseCase, statement GetStatementUseCase) *Account {
	return &Account{balance: balance, statement: statement}
}

// Balance: GET /v1/balance
func (h *Account) Balance(w http.ResponseWriter, r *http.Request) {
	balances, err := h.balance.Execute(r.Context(), middleware.MerchantID(r.Context()))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"balances": balances})
}

// Statement: GET /v1/statement?limit=50&before=<id>
func (h *Account) Statement(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	before, err := intParam(r, "before")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	page, err := h.statement.Execute(r.Context(), middleware.MerchantID(r.Context()), int64(before), limit)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// intParam lê um inteiro opcional da query string (ausente = 0).
func intParam(r *http.Request, name string) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, &paramError{name: name}
	}
	return n, nil
}

type paramError struct{ name string }

func (e *paramError) Error() string { return "parâmetro " + e.name + " deve ser um inteiro" }
