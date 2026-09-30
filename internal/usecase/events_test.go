package usecase_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/payment"
	"github.com/VictorXdAugusto/payment-gateway-go/internal/usecase"
)

func TestEvents_ApprovedPaymentEmitsCreatedThenAuthorized(t *testing.T) {
	e := setup(t)
	e.created(t, "k1", 5000)

	want := []string{"payment.created", "payment.authorized"}
	if got := e.eventTypes(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("eventos = %v, want %v", got, want)
	}
}

func TestEvents_DeclinedPaymentEmitsCreatedThenFailedWithReason(t *testing.T) {
	e := setup(t)
	e.psp.script(modeDecline)
	e.created(t, "k1", 4000)

	if got, want := e.eventTypes(t), []string{"payment.created", "payment.failed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("eventos = %v, want %v", got, want)
	}
	var payload []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT payload FROM outbox_events WHERE type = 'payment.failed'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var env struct {
		Data struct{ Reason string }
	}
	if err := json.Unmarshal(payload, &env); err != nil || env.Data.Reason != "insufficient_funds" {
		t.Errorf("payload = %s (%v)", payload, err)
	}
}

// unknown é um detalhe interno: o lojista só sabe que o pagamento existe (created).
func TestEvents_UnknownPaymentEmitsOnlyCreated(t *testing.T) {
	e := setup(t)
	e.psp.script(modeTimeoutLost)
	e.created(t, "k1", 5000)

	if got, want := e.eventTypes(t), []string{"payment.created"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("eventos = %v, want %v", got, want)
	}
}

func TestEvents_ResolvingUnknownEmitsTheOutcome(t *testing.T) {
	e := setup(t)
	e.psp.script(modeTimeoutHappened)
	id := e.created(t, "k1", 5000)

	if _, err := e.resolve.Execute(e.ctx, payment.ID(id)); err != nil {
		t.Fatal(err)
	}
	if got, want := e.eventTypes(t), []string{"payment.created", "payment.authorized"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("eventos = %v, want %v", got, want)
	}
	// Uma segunda passada do reconciliador não repete o evento.
	if _, err := e.resolve.Execute(e.ctx, payment.ID(id)); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, "outbox_events"); n != 2 {
		t.Errorf("eventos = %d, want 2", n)
	}
}

func TestEvents_CaptureEmitsCaptured(t *testing.T) {
	e := setup(t)
	id := e.created(t, "k1", 10000)
	if _, err := e.capture.Execute(e.ctx, e.capIn(id, "cap-1")); err != nil {
		t.Fatal(err)
	}
	want := []string{"payment.created", "payment.authorized", "payment.captured"}
	if got := e.eventTypes(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("eventos = %v, want %v", got, want)
	}
}

// O contrato do que o lojista recebe.
func TestEvents_PayloadContract(t *testing.T) {
	e := setup(t)
	id := e.created(t, "k1", 10000)
	if _, err := e.capture.Execute(e.ctx, e.capIn(id, "cap-1")); err != nil {
		t.Fatal(err)
	}

	var (
		eventID, typ, merchant, paymentID string
		payload                           []byte
	)
	err := e.pool.QueryRow(e.ctx, `SELECT event_id, type, merchant_id::text, payment_id, payload
		FROM outbox_events WHERE type = 'payment.captured'`).Scan(&eventID, &typ, &merchant, &paymentID, &payload)
	if err != nil {
		t.Fatal(err)
	}

	var env struct {
		ID        string    `json:"id"`
		Type      string    `json:"type"`
		CreatedAt time.Time `json:"created_at"`
		Data      struct {
			Payment  usecase.PaymentView `json:"payment"`
			Amount   int64               `json:"amount"`
			Currency string              `json:"currency"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatal(err)
	}
	if env.ID != eventID || env.Type != typ {
		t.Errorf("o id/tipo do corpo (%s/%s) têm que bater com a linha (%s/%s): é o que o lojista deduplica", env.ID, env.Type, eventID, typ)
	}
	if merchant != e.merchant || paymentID != id {
		t.Errorf("merchant=%s payment=%s", merchant, paymentID)
	}
	if env.Data.Payment.ID != id || env.Data.Payment.Status != "captured" || env.Data.Amount != 10000 || env.Data.Currency != "BRL" {
		t.Errorf("data = %+v", env.Data)
	}
	if !env.CreatedAt.Equal(t0) {
		t.Errorf("created_at = %v, want o instante do fato (%v)", env.CreatedAt, t0)
	}
}

// ---------------------------------------------------------- atomicidade

func TestEvents_OutboxFailureOnCreate_RollsBackThePayment(t *testing.T) {
	e := setup(t)
	e.outbox.fail.Store(true)

	if _, err := e.create.Execute(e.ctx, e.in("k1", 5000)); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v", err)
	}
	if e.count(t, "payments") != 0 || e.count(t, "outbox_events") != 0 {
		t.Fatalf("pagamentos=%d eventos=%d: estado e evento têm que andar juntos", e.count(t, "payments"), e.count(t, "outbox_events"))
	}
	if calls, _, _, _ := e.psp.stats(); calls != 0 {
		t.Errorf("PSP foi chamado %d vezes para um pagamento que não existe", calls)
	}

	e.outbox.fail.Store(false)
	if _, err := e.create.Execute(e.ctx, e.in("k1", 5000)); err != nil {
		t.Fatalf("o retry deveria funcionar: %v", err)
	}
}

// O PSP aprovou, mas gravar o desfecho + o evento falhou: nada do desfecho persiste; o retry recupera.
func TestEvents_OutboxFailureAfterPSPApproved_KeepsPaymentCreated_AndRetryRecovers(t *testing.T) {
	e := setup(t)
	e.created(t, "warmup", 100) // consome o id pay_1 e mostra que o resto não é afetado
	before := e.count(t, "outbox_events")

	// Falha apenas na 2ª gravação de evento desta requisição (a da fase do PSP).
	var calls int
	var mu sync.Mutex
	e.outbox.hook = func() bool { mu.Lock(); defer mu.Unlock(); calls++; return calls == 2 }

	if _, err := e.create.Execute(e.ctx, e.in("k2", 5000)); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v", err)
	}
	if got := e.load(t, "pay_2").Status(); got != payment.StatusCreated {
		t.Fatalf("status = %s: o desfecho do PSP não pode ter sido gravado sem o evento", got)
	}
	if n := e.count(t, "outbox_events") - before; n != 1 { // só o payment.created da fase 1
		t.Errorf("eventos novos = %d, want 1", n)
	}

	if _, err := e.create.Execute(e.ctx, e.in("k2", 5000)); err != nil {
		t.Fatal(err)
	}
	if got, want := e.eventTypes(t)[before:], []string{"payment.created", "payment.authorized"}; !reflect.DeepEqual(got, want) {
		t.Errorf("eventos = %v, want %v (sem duplicar o created)", got, want)
	}
}

func TestEvents_OutboxFailureOnCapture_RollsBackEverything(t *testing.T) {
	e := setup(t)
	id := e.created(t, "k1", 10000)
	eventsBefore := e.count(t, "outbox_events")
	e.outbox.fail.Store(true)

	if _, err := e.capture.Execute(e.ctx, e.capIn(id, "cap-1")); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v", err)
	}
	if got := e.load(t, id).Status(); got != payment.StatusAuthorized {
		t.Errorf("status = %s: capturado sem evento", got)
	}
	e.assertLedger(t, 0, 0, 0) // o ledger também desfez
	if n := e.count(t, "outbox_events"); n != eventsBefore {
		t.Errorf("eventos = %d, want %d", n, eventsBefore)
	}
}

func TestEvents_OutboxFailureOnResolve_KeepsThePaymentUnknown(t *testing.T) {
	e := setup(t)
	e.psp.script(modeTimeoutHappened)
	id := e.created(t, "k1", 5000)
	e.outbox.fail.Store(true)

	if _, err := e.resolve.Execute(e.ctx, payment.ID(id)); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v", err)
	}
	if got := e.load(t, id).Status(); got != payment.StatusUnknown {
		t.Errorf("status = %s: resolvido sem evento", got)
	}
	e.outbox.fail.Store(false)
	if status, err := e.resolve.Execute(e.ctx, payment.ID(id)); err != nil || status != payment.StatusAuthorized {
		t.Errorf("a próxima rodada deveria resolver: %s / %v", status, err)
	}
}

// Retries de criação e replays não geram eventos extras.
func TestEvents_RetriesAndReplaysDoNotDuplicateEvents(t *testing.T) {
	e := setup(t)
	e.created(t, "k1", 5000)
	for i := 0; i < 3; i++ {
		if _, err := e.create.Execute(e.ctx, e.in("k1", 5000)); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.count(t, "outbox_events"); n != 2 {
		t.Errorf("eventos = %d, want 2 (created + authorized)", n)
	}
}

func TestEvents_ConcurrentSameKey_EmitsEachEventOnce(t *testing.T) {
	e := setup(t)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = e.create.Execute(e.ctx, e.in("racy", 7777))
		}()
	}
	close(start)
	wg.Wait()

	if got, want := e.eventTypes(t), []string{"payment.created", "payment.authorized"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("eventos = %v, want %v", got, want)
	}
}
