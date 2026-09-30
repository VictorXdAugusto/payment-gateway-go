package observability

import (
	"context"
	"errors"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/psp"
)

// InstrumentedGateway decora um psp.Gateway medindo cada chamada. O resto do sistema não sabe
// que está sendo medido: a instrumentação entra na composição (main), não nos casos de uso.
type InstrumentedGateway struct {
	psp.Gateway
	m *Metrics
}

var _ psp.Gateway = (*InstrumentedGateway)(nil)

func InstrumentGateway(g psp.Gateway, m *Metrics) *InstrumentedGateway {
	return &InstrumentedGateway{Gateway: g, m: m}
}

func (g *InstrumentedGateway) Authorize(ctx context.Context, req psp.AuthorizeRequest) (psp.Authorization, error) {
	start := time.Now()
	out, err := g.Gateway.Authorize(ctx, req)
	g.m.PSPCall("authorize", pspOutcome(err), time.Since(start))
	return out, err
}

func (g *InstrumentedGateway) Capture(ctx context.Context, req psp.CaptureRequest) error {
	start := time.Now()
	err := g.Gateway.Capture(ctx, req)
	g.m.PSPCall("capture", pspOutcome(err), time.Since(start))
	return err
}

func (g *InstrumentedGateway) Void(ctx context.Context, req psp.VoidRequest) error {
	start := time.Now()
	err := g.Gateway.Void(ctx, req)
	g.m.PSPCall("void", pspOutcome(err), time.Since(start))
	return err
}

func (g *InstrumentedGateway) Refund(ctx context.Context, req psp.RefundRequest) error {
	start := time.Now()
	err := g.Gateway.Refund(ctx, req)
	g.m.PSPCall("refund", pspOutcome(err), time.Since(start))
	return err
}

func (g *InstrumentedGateway) Lookup(ctx context.Context, key string) (psp.LookupResult, error) {
	start := time.Now()
	out, err := g.Gateway.Lookup(ctx, key)
	g.m.PSPCall("lookup", pspOutcome(err), time.Since(start))
	return out, err
}

func pspOutcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, psp.ErrDeclined):
		return "declined"
	case errors.Is(err, psp.ErrIndeterminate):
		return "indeterminate"
	default:
		return "error"
	}
}
