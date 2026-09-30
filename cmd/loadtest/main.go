// Comando loadtest estressa a API de um gateway em execução e VERIFICA os invariantes sob
// concorrência: não é só "quantas requisições por segundo", é "o dinheiro continua certo".
//
// Fases:
//  1. vazão: cria N pagamentos em paralelo e mede latência (p50/p95/p99);
//  2. tempestade de idempotência: a MESMA chave disparada R vezes ao mesmo tempo gera UM pagamento;
//  3. corrida de captura: M capturas simultâneas com chaves diferentes, exatamente uma vence;
//  4. conservação no estorno: estornos simultâneos nunca passam do capturado e o saldo do
//     lojista fecha com a conta esperada.
//
// Sai com código 1 se algum invariante quebrar.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type client struct {
	base, apiKey string
	http         *http.Client
}

type result struct {
	status   int
	body     []byte
	replayed bool
	elapsed  time.Duration
}

func (c *client) do(method, path, idemKey string, body any) (result, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	start := time.Now()
	res, err := c.http.Do(req)
	if err != nil {
		return result{}, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return result{status: res.StatusCode, body: b, replayed: res.Header.Get("Idempotent-Replayed") == "true",
		elapsed: time.Since(start)}, nil
}

type payment struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	RefundedAmount int64  `json:"refunded_amount"`
}

func parse(r result) payment {
	var p payment
	_ = json.Unmarshal(r.body, &p)
	return p
}

var failures atomic.Int32

func check(ok bool, format string, args ...any) {
	if !ok {
		failures.Add(1)
		fmt.Printf("  FALHOU: "+format+"\n", args...)
	}
}

func main() {
	var (
		base        = flag.String("url", "http://localhost:8090", "URL base da API")
		apiKey      = flag.String("api-key", os.Getenv("API_KEY"), "API key do lojista (ou env API_KEY)")
		concurrency = flag.Int("concurrency", 32, "requisições simultâneas na fase de vazão")
		payments    = flag.Int("payments", 500, "pagamentos criados na fase de vazão")
		storms      = flag.Int("storms", 20, "chaves na tempestade de idempotência")
		burst       = flag.Int("burst", 30, "requisições simultâneas por corrida")
		races       = flag.Int("races", 10, "corridas de captura e de estorno")
	)
	flag.Parse()
	if *apiKey == "" {
		fmt.Println("informe -api-key ou API_KEY (make merchant NAME=carga cria um lojista)")
		os.Exit(2)
	}
	c := &client{base: strings.TrimRight(*base, "/"), apiKey: *apiKey,
		http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 256}}}
	run := fmt.Sprintf("lt%d", time.Now().UnixNano())

	before := balance(c)
	phaseThroughput(c, run, *concurrency, *payments)
	phaseIdempotencyStorm(c, run, *storms, *burst)
	captured := phaseCaptureRace(c, run, *races, *burst)
	refunded := phaseRefundConservation(c, run, *races)

	// Saldo: cada captura de R$ 100,00 rende 9710 ao lojista; cada pagamento estornado por
	// inteiro termina em -290 (a taxa não volta). Valores redondos evitam ruído de arredondamento.
	want := int64(captured)*9710 + int64(refunded)*-290
	got := balance(c) - before
	fmt.Println("\nfase 5: saldo do lojista")
	check(got == want, "variação do saldo = %d, esperado %d", got, want)
	fmt.Printf("  variação %d (esperado %d)\n", got, want)

	if n := failures.Load(); n > 0 {
		fmt.Printf("\n%d invariante(s) quebrado(s)\n", n)
		os.Exit(1)
	}
	fmt.Println("\ntodos os invariantes se mantiveram")
}

func balance(c *client) int64 {
	r, err := c.do("GET", "/v1/balance", "", nil)
	if err != nil || r.status != 200 {
		fmt.Println("não foi possível ler o saldo:", err, string(r.body))
		os.Exit(2)
	}
	var out struct {
		Balances []struct {
			Currency string
			Amount   int64
		}
	}
	_ = json.Unmarshal(r.body, &out)
	for _, b := range out.Balances {
		if b.Currency == "BRL" {
			return b.Amount
		}
	}
	return 0
}

func create(c *client, key string, amount int64) (result, error) {
	return c.do("POST", "/v1/payments", key, map[string]any{"amount": amount, "currency": "BRL"})
}

func phaseThroughput(c *client, run string, concurrency, total int) {
	fmt.Printf("fase 1: vazão (%d pagamentos, %d em paralelo)\n", total, concurrency)
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		latencies []time.Duration
		codes     = map[int]int{}
		next      atomic.Int64
	)
	start := time.Now()
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := next.Add(1)
				if i > int64(total) {
					return
				}
				r, err := create(c, fmt.Sprintf("%s-tp-%d", run, i), 1000)
				mu.Lock()
				if err != nil {
					codes[0]++
				} else {
					codes[r.status]++
					latencies = append(latencies, r.elapsed)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(p float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		return latencies[int(float64(len(latencies)-1)*p)]
	}
	fmt.Printf("  %.0f req/s | p50 %s | p95 %s | p99 %s | status %v\n",
		float64(total)/elapsed.Seconds(), pct(.5).Round(time.Millisecond), pct(.95).Round(time.Millisecond),
		pct(.99).Round(time.Millisecond), codes)
	check(codes[201] == total, "%d de %d criações devolveram 201", codes[201], total)
}

func phaseIdempotencyStorm(c *client, run string, keys, burst int) {
	fmt.Printf("fase 2: tempestade de idempotência (%d chaves x %d requisições simultâneas)\n", keys, burst)
	for k := 0; k < keys; k++ {
		key := fmt.Sprintf("%s-storm-%d", run, k)
		var wg sync.WaitGroup
		results := make([]result, burst)
		start := make(chan struct{})
		for i := 0; i < burst; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i], _ = create(c, key, 2000)
			}()
		}
		close(start)
		wg.Wait()

		ids := map[string]bool{}
		for _, r := range results {
			switch r.status {
			case 201:
				ids[parse(r).ID] = true
			case 409: // outra requisição com esta chave ainda em processamento: resposta válida
			default:
				check(false, "chave %d: status inesperado %d %s", k, r.status, r.body)
			}
		}
		check(len(ids) == 1, "chave %d gerou %d pagamentos distintos, esperado 1", k, len(ids))
	}
	fmt.Println("  cada chave gerou exatamente um pagamento")
}

func phaseCaptureRace(c *client, run string, races, burst int) int {
	fmt.Printf("fase 3: corrida de captura (%d pagamentos x %d capturas simultâneas)\n", races, burst)
	captured := 0
	for n := 0; n < races; n++ {
		r, err := create(c, fmt.Sprintf("%s-cap-%d", run, n), 10000)
		if err != nil || r.status != 201 {
			check(false, "criar pagamento da corrida %d: %v %d", n, err, r.status)
			continue
		}
		id := parse(r).ID

		var wg sync.WaitGroup
		var wins atomic.Int32
		start := make(chan struct{})
		for i := 0; i < burst; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				res, _ := c.do("POST", "/v1/payments/"+id+"/capture", fmt.Sprintf("%s-cap-%d-%d", run, n, i), nil)
				if res.status == 200 && !res.replayed {
					wins.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		check(wins.Load() == 1, "pagamento %s: %d capturas venceram, esperado 1", id, wins.Load())
		if wins.Load() == 1 {
			captured++
		}
	}
	fmt.Printf("  %d de %d pagamentos capturados exatamente uma vez\n", captured, races)
	return captured
}

func phaseRefundConservation(c *client, run string, races int) int {
	const parts = 10
	fmt.Printf("fase 4: conservação no estorno (%d pagamentos x %d estornos simultâneos de R$ 10,00)\n", races, parts)
	refunded := 0
	for n := 0; n < races; n++ {
		r, err := create(c, fmt.Sprintf("%s-ref-%d", run, n), 10000)
		if err != nil || r.status != 201 {
			check(false, "criar pagamento %d: %v %d", n, err, r.status)
			continue
		}
		id := parse(r).ID
		if res, _ := c.do("POST", "/v1/payments/"+id+"/capture", fmt.Sprintf("%s-refcap-%d", run, n), nil); res.status != 200 {
			check(false, "capturar %s: %d %s", id, res.status, res.body)
			continue
		}

		var wg sync.WaitGroup
		var failed atomic.Int32
		start := make(chan struct{})
		for i := 0; i < parts; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				key := fmt.Sprintf("%s-ref-%d-%d", run, n, i)
				// 409 = perdeu a corrida do lock otimista; repetir com a MESMA chave cura (o PSP é idempotente).
				for attempt := 0; attempt < 60; attempt++ {
					res, err := c.do("POST", "/v1/payments/"+id+"/refund", key, map[string]any{"amount": 1000})
					if err == nil && res.status == 200 {
						return
					}
					if err == nil && res.status != 409 {
						failed.Add(1)
						return
					}
					time.Sleep(time.Duration(5+attempt) * time.Millisecond)
				}
				failed.Add(1)
			}()
		}
		close(start)
		wg.Wait()

		g, _ := c.do("GET", "/v1/payments/"+id, "", nil)
		p := parse(g)
		check(failed.Load() == 0, "pagamento %s: %d estornos falharam", id, failed.Load())
		check(p.Status == "refunded" && p.RefundedAmount == 10000, "pagamento %s: status=%s estornado=%d, esperado refunded e 10000", id, p.Status, p.RefundedAmount)
		if p.Status == "refunded" && p.RefundedAmount == 10000 {
			refunded++
		}
	}
	fmt.Printf("  %d de %d pagamentos estornados por inteiro, sem passar do capturado\n", refunded, races)
	return refunded
}
