// Package psp implementa o cliente HTTP do adquirente com timeout por tentativa,
// retry com backoff exponencial + jitter e classificação de erros.
package psp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"time"

	domain "github.com/VictorXdAugusto/payment-gateway-go/internal/psp"
)

type Config struct {
	BaseURL        string
	AttemptTimeout time.Duration // limite de UMA tentativa
	MaxAttempts    int           // tentativas totais (1 = sem retry)
	BaseBackoff    time.Duration // espera base entre tentativas (dobra a cada uma)
	MaxBackoff     time.Duration
}

type Client struct {
	cfg  Config
	http *http.Client
	// sleep e jitter são injetáveis para os testes não dependerem de relógio nem de sorte.
	sleep  func(ctx context.Context, d time.Duration) error
	jitter func(max time.Duration) time.Duration
}

var _ domain.Gateway = (*Client)(nil)

func New(cfg Config) *Client {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	return &Client{
		cfg: cfg,
		// O timeout é por tentativa, via context. Redirect NUNCA é seguido: um 302 transformaria o
		// POST de captura num GET que "funciona" sem capturar nada (e o 3xx sobe como erro).
		http:   &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		sleep:  sleep,
		jitter: fullJitter,
	}
}

func (c *Client) Authorize(ctx context.Context, req domain.AuthorizeRequest) (domain.Authorization, error) {
	var out struct {
		ID string `json:"id"`
	}
	status, body, err := c.do(ctx, http.MethodPost, "/v1/authorizations", req.IdempotencyKey,
		map[string]any{"amount": req.Amount.Amount(), "currency": req.Amount.Currency()})
	if err != nil {
		return domain.Authorization{}, err
	}
	if err := interpret(status, body, &out); err != nil {
		return domain.Authorization{}, err
	}
	if out.ID == "" {
		return domain.Authorization{}, fmt.Errorf("%w: resposta sem id", domain.ErrIndeterminate)
	}
	return domain.Authorization{Reference: out.ID}, nil
}

func (c *Client) Capture(ctx context.Context, req domain.CaptureRequest) error {
	status, body, err := c.do(ctx, http.MethodPost, "/v1/authorizations/"+url.PathEscape(req.Reference)+"/capture",
		req.IdempotencyKey, map[string]any{"amount": req.Amount.Amount()})
	if err != nil {
		return err
	}
	return interpret(status, body, nil)
}

func (c *Client) Void(ctx context.Context, req domain.VoidRequest) error {
	status, body, err := c.do(ctx, http.MethodPost, "/v1/authorizations/"+url.PathEscape(req.Reference)+"/void",
		req.IdempotencyKey, map[string]any{})
	if err != nil {
		return err
	}
	return interpret(status, body, nil)
}

func (c *Client) Refund(ctx context.Context, req domain.RefundRequest) error {
	status, body, err := c.do(ctx, http.MethodPost, "/v1/authorizations/"+url.PathEscape(req.Reference)+"/refund",
		req.IdempotencyKey, map[string]any{"amount": req.Amount.Amount()})
	if err != nil {
		return err
	}
	return interpret(status, body, nil)
}

func (c *Client) Lookup(ctx context.Context, key string) (domain.LookupResult, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/v1/authorizations?idempotency_key="+url.QueryEscape(key), "", nil)
	if err != nil {
		return domain.LookupResult{}, err
	}
	if status == http.StatusNotFound {
		return domain.LookupResult{Outcome: domain.OutcomeNotFound}, nil
	}
	var out struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		DeclineCode string `json:"decline_code"`
	}
	if err := interpret(status, body, &out); err != nil {
		return domain.LookupResult{}, err
	}
	switch domain.Outcome(out.Status) {
	case domain.OutcomeAuthorized:
		return domain.LookupResult{Outcome: domain.OutcomeAuthorized, Reference: out.ID}, nil
	case domain.OutcomeDeclined:
		return domain.LookupResult{Outcome: domain.OutcomeDeclined, DeclineCode: out.DeclineCode}, nil
	}
	return domain.LookupResult{}, fmt.Errorf("%w: status %q desconhecido", domain.ErrIndeterminate, out.Status)
}

// do faz a chamada com retry. Só repete o que é seguro repetir: erro de rede, timeout e
// 429/5xx. É seguro PORQUE o PSP é idempotente pela Idempotency-Key, que vai em TODAS as tentativas.
//
// Devolve (status, corpo) da resposta final. Se esgotar as tentativas sem resposta
// utilizável, devolve ErrIndeterminate: a operação pode ter acontecido.
func (c *Client) do(ctx context.Context, method, path, idemKey string, payload any) (int, []byte, error) {
	var raw []byte
	if payload != nil {
		var err error
		if raw, err = json.Marshal(payload); err != nil {
			return 0, nil, err
		}
	}

	var lastErr error
	for attempt := 0; attempt < c.cfg.MaxAttempts; attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, c.backoff(attempt)); err != nil {
				return 0, nil, err // o chamador desistiu: não é "indeterminado", é cancelamento
			}
		}

		status, body, err := c.once(ctx, method, path, idemKey, raw)
		switch {
		case ctx.Err() != nil:
			return 0, nil, ctx.Err()
		case err != nil:
			lastErr = err // rede/timeout: tenta de novo
		case retryable(status):
			lastErr = fmt.Errorf("PSP respondeu %d", status)
		default:
			return status, body, nil
		}
	}
	return 0, nil, fmt.Errorf("%w: %d tentativas: %v", domain.ErrIndeterminate, c.cfg.MaxAttempts, lastErr)
}

func (c *Client) once(ctx context.Context, method, path, idemKey string, raw []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.AttemptTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return res.StatusCode, body, nil
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// backoff: exponencial com jitter total ("full jitter"). Sem jitter, mil clientes que
// falharam juntos tentam de novo juntos e derrubam o PSP de novo (thundering herd).
func (c *Client) backoff(attempt int) time.Duration {
	ceil := c.cfg.BaseBackoff << (attempt - 1)
	if c.cfg.MaxBackoff > 0 && (ceil > c.cfg.MaxBackoff || ceil <= 0) {
		ceil = c.cfg.MaxBackoff
	}
	return c.jitter(ceil)
}

// interpret traduz a resposta final: 2xx preenche out; 402 é recusa definitiva; o resto é erro.
func interpret(status int, body []byte, out any) error {
	switch {
	case status >= 200 && status < 300:
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("%w: resposta ilegível: %v", domain.ErrIndeterminate, err)
		}
		return nil
	case status == http.StatusPaymentRequired:
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		return &domain.DeclinedError{Code: e.Error.Code, Message: e.Error.Message}
	default:
		// 4xx que não é recusa: requisição nossa errada. Não é indeterminado (o PSP respondeu
		// com clareza) nem recusa de negócio: é bug/contrato, sobe como erro comum.
		return fmt.Errorf("PSP rejeitou a requisição (%d): %s", status, bytes.TrimSpace(body))
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func fullJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(max) + 1))
}

// IsIndeterminate ajuda quem só tem o erro na mão.
func IsIndeterminate(err error) bool { return errors.Is(err, domain.ErrIndeterminate) }
