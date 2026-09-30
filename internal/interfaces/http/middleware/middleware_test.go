package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/interfaces/http/middleware"
)

func TestObserve_RecoversFromPanic(t *testing.T) {
	h := middleware.Observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("bug no handler")
	}))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "internal_error") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "bug no handler") {
		t.Error("o detalhe do pânico vazou para o cliente")
	}
}

func TestObserve_PropagatesIncomingRequestID(t *testing.T) {
	h := middleware.Observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-Id", "abc-123")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-Id"); got != "abc-123" {
		t.Errorf("X-Request-Id = %q", got)
	}
}
