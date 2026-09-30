package handler_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/interfaces/http/handler"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestHealth(t *testing.T) {
	tests := []struct {
		name       string
		pingErr    error
		call       func(*handler.Health) http.HandlerFunc
		wantStatus int
	}{
		{"live sempre ok", errors.New("banco fora"), func(h *handler.Health) http.HandlerFunc { return h.Live }, http.StatusOK},
		{"ready com banco ok", nil, func(h *handler.Health) http.HandlerFunc { return h.Ready }, http.StatusOK},
		{"ready com banco fora", errors.New("banco fora"), func(h *handler.Health) http.HandlerFunc { return h.Ready }, http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := handler.NewHealth(fakePinger{err: tt.pingErr})
			rec := httptest.NewRecorder()

			tt.call(h)(rec, httptest.NewRequest(http.MethodGet, "/", nil))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}
