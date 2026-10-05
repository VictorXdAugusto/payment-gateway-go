// Package webhook entrega eventos ao endpoint do lojista com assinatura e defesa contra SSRF.
package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"

	"github.com/VictorXdAugusto/payment-gateway-go/internal/outbox"
	sig "github.com/VictorXdAugusto/payment-gateway-go/internal/webhook"
)

// ErrBlockedDestination: o endereço do webhook aponta para uma rede que não é a internet pública.
var ErrBlockedDestination = errors.New("destino do webhook bloqueado")

// StatusError: o endpoint respondeu, mas não com 2xx.
type StatusError struct {
	Code    int
	Snippet string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("endpoint respondeu %d: %s", e.Code, e.Snippet)
}

// defaultTimeout é o limite de uma entrega quando Config.Timeout não é positivo. Timeout zero no
// http.Client significa "sem limite": um endpoint que aceita a conexão e nunca responde prenderia
// a entrega para sempre. Variável (e não constante) só para o teste encurtá-lo.
var defaultTimeout = 10 * time.Second

type Config struct {
	Timeout time.Duration // limite total de UMA entrega; <= 0 usa o padrão de 10s
	// AllowPrivate desliga a proteção contra SSRF e o exigir-HTTPS. SÓ para desenvolvimento
	// e testes (o receptor de exemplo roda numa rede privada do Docker).
	AllowPrivate bool
}

type Sender struct {
	client *http.Client
	cfg    Config
	now    func() time.Time
}

func NewSender(cfg Config) *Sender {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	dialer := &net.Dialer{
		Timeout: 3 * time.Second,
		// Control roda DEPOIS da resolução de DNS, com o IP real que vai ser conectado. Checar
		// aqui (e não a URL antes) fecha o DNS rebinding: um domínio que resolve para IP público
		// na validação e para 169.254.169.254 na conexão é barrado do mesmo jeito.
		Control: func(_, address string, _ syscall.RawConn) error {
			if cfg.AllowPrivate {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || !IsPublicIP(ip) {
				return fmt.Errorf("%w: %s", ErrBlockedDestination, host)
			}
			return nil
		},
	}
	return &Sender{
		cfg: cfg,
		now: time.Now,
		client: &http.Client{
			Timeout: cfg.Timeout,
			Transport: &http.Transport{
				Proxy:                 nil, // proxy de ambiente contornaria o controle do dialer
				DialContext:           dialer.DialContext,
				TLSHandshakeTimeout:   3 * time.Second,
				ResponseHeaderTimeout: cfg.Timeout,
				MaxIdleConns:          50,
				IdleConnTimeout:       30 * time.Second,
			},
			// Redirect é o truque clássico para saltar a validação do destino: não seguimos.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Deliver envia UMA tentativa. nil = o endpoint respondeu 2xx.
// A assinatura é gerada AGORA (não na criação do evento): tentativas horas depois continuam
// dentro da tolerância de timestamp do receptor.
func (s *Sender) Deliver(ctx context.Context, d outbox.Delivery) error {
	// Segredo vazio assinaria com chave HMAC vazia, reproduzível por qualquer um que conheça o
	// endpoint: um atacante forjaria eventos de pagamento. Nada é enviado.
	if d.Secret == "" {
		return fmt.Errorf("%w: segredo do webhook não configurado", outbox.ErrPermanent)
	}
	if err := s.validateURL(d.WebhookURL); err != nil {
		return fmt.Errorf("%w: %v", outbox.ErrPermanent, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.WebhookURL, bytes.NewReader(d.Event.Payload))
	if err != nil {
		return fmt.Errorf("%w: %v", outbox.ErrPermanent, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "payment-gateway-go-webhook/1")
	req.Header.Set(sig.EventIDHeader, d.Event.EventID)
	req.Header.Set(sig.SignatureHeader, sig.Sign(d.Secret, s.now(), d.Event.Payload))

	res, err := s.client.Do(req)
	if err != nil {
		// *url.Error carrega a URL completa, que pode ter token na query string, e o erro vai para
		// log e outbox_events.last_error. Mantém só a operação e a causa (errors.Is/As seguem valendo).
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
		}
		if errors.Is(err, ErrBlockedDestination) {
			return fmt.Errorf("%w: %w", outbox.ErrPermanent, err)
		}
		return err
	}
	defer res.Body.Close()

	snippet, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10)) // reaproveita a conexão sem ler o mundo

	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	return &StatusError{Code: res.StatusCode, Snippet: string(bytes.TrimSpace(snippet))}
}

func (s *Sender) validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("URL inválida %q", raw)
	}
	if u.User != nil {
		return errors.New("URL com credenciais embutidas")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !s.cfg.AllowPrivate {
			return errors.New("webhook exige https")
		}
	default:
		return fmt.Errorf("esquema %q não suportado", u.Scheme)
	}
	return nil
}

var blockedPrefixes = mustPrefixes(
	"0.0.0.0/8",       // "esta rede"
	"100.64.0.0/10",   // CGNAT
	"192.0.0.0/24",    // IETF
	"198.18.0.0/15",   // benchmark
	"240.0.0.0/4",     // reservado
	"64:ff9b::/96",    // NAT64
	"fec0::/10",       // IPv6 site-local (obsoleto)
	"2002::/16",       // 6to4: pode embutir IPv4 privado
	"2001::/32",       // Teredo
	"2001:db8::/32",   // documentação IPv6
	"192.0.2.0/24",    // documentação IPv4 (TEST-NET-1)
	"198.51.100.0/24", // documentação IPv4 (TEST-NET-2)
	"203.0.113.0/24",  // documentação IPv4 (TEST-NET-3)
	"100::/64",        // discard-only
	"192.88.99.0/24",  // 6to4 relay anycast (obsoleto)
)

func mustPrefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(cidrs))
	for i, c := range cidrs {
		out[i] = netip.MustParsePrefix(c)
	}
	return out
}

// IsPublicIP diz se o endereço pertence à internet pública. Tudo o mais (loopback, redes
// privadas, link-local incluindo o endpoint de metadados de nuvem 169.254.169.254, CGNAT,
// multicast, reservados) é destino proibido.
func IsPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap() // ::ffff:127.0.0.1 é o loopback disfarçado de IPv6
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
