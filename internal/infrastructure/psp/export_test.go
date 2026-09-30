package psp

import (
	"context"
	"time"
)

// NewForTest permite injetar sleep e jitter para testar o backoff sem relógio nem sorte.
func NewForTest(cfg Config, sleep func(context.Context, time.Duration) error, jitter func(time.Duration) time.Duration) *Client {
	c := New(cfg)
	c.sleep, c.jitter = sleep, jitter
	return c
}
