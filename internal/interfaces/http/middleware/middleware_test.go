package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

type recorded struct {
	method, route string
	status        int
	d             time.Duration
}

type fakeRecorder struct{ got []recorded }

func (f *fakeRecorder) HTTPRequest(method, route string, status int, d time.Duration) {
	f.got = append(f.got, recorded{method, route, status, d})
}

// A rota medida é o PADRÃO do mux, não o caminho cru: dois ids diferentes caem na mesma série.
func TestObserveWith_RecordsTheMuxPatternNotTheRawPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/payments/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	rec := &fakeRecorder{}
	srv := httptest.NewServer(middleware.ObserveWith(rec, mux))
	defer srv.Close()

	for _, path := range []string{"/v1/payments/pay_1", "/v1/payments/pay_2", "/nao/existe"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	if len(rec.got) != 3 {
		t.Fatalf("medições = %d, want 3", len(rec.got))
	}
	for _, r := range rec.got[:2] {
		if r.route != "GET /v1/payments/{id}" || r.status != http.StatusTeapot || r.method != "GET" {
			t.Errorf("medição = %+v", r)
		}
	}
	if rec.got[2].route != "unmatched" || rec.got[2].status != http.StatusNotFound {
		t.Errorf("rota inexistente = %+v, want unmatched/404", rec.got[2])
	}
}

func TestObserveWith_RecordsPanicsAs500(t *testing.T) {
	rec := &fakeRecorder{}
	h := middleware.ObserveWith(rec, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("bug") }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if len(rec.got) != 1 || rec.got[0].status != http.StatusInternalServerError {
		t.Errorf("medições = %+v, want uma de 500", rec.got)
	}
}
