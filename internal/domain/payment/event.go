package payment

import (
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/domain/money"
)

type EventType string

const (
	EventCreated    EventType = "payment.created"
	EventAuthorized EventType = "payment.authorized"
	EventCaptured   EventType = "payment.captured"
	EventVoided     EventType = "payment.voided"
	EventFailed     EventType = "payment.failed"
	EventRefunded   EventType = "payment.refunded"
)

// Event é um fato que já aconteceu. O agregado os acumula e a camada de aplicação
// os grava na outbox, na mesma transação do estado (passo 6).
type Event struct {
	Type       EventType
	PaymentID  ID
	MerchantID MerchantID
	Amount     money.Money // valor do fato (no estorno, o valor daquele estorno)
	Reason     string      // preenchido em payment.failed
	OccurredAt time.Time
}
