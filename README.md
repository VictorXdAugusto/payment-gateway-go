# payment-gateway-go

Gateway de pagamentos em Go, no estilo de um mini-Stripe: autorização, captura, cancelamento,
estorno, saldo e extrato do lojista, com entrega de webhooks.

## Objetivo

É um case de estudo sobre como tratar dinheiro quando a rede falha. O caminho feliz de um
pagamento é simples. O difícil é o que acontece quando o cliente repete uma requisição que deu
timeout, quando o processo cai depois de cobrar no adquirente ou quando o adquirente não responde.
O projeto existe para resolver esses casos sem cobrar duas vezes, sem perder cobrança e sem deixar
o saldo errado.

As técnicas aplicadas são **idempotência, máquina de estados, ledger de partida dobrada, chamada
ao adquirente fora da transação, outbox transacional, webhooks assinados e reconciliação**.

O adquirente (PSP) é um simulador que roda como serviço HTTP real e falha de propósito
(timeout, 503, aprovação perdida), para que cada garantia abaixo seja provada por teste,
não só afirmada.

Não é um produto pronto para produção. O que ficou de fora está em
[Limitações conhecidas](#limitações-conhecidas).

## Stack

Go 1.27 com a biblioteca padrão no HTTP (`net/http`, `log/slog`), sem framework web. `pgx/v5`,
PostgreSQL 17, migrations SQL com golang-migrate, métricas com Prometheus e Docker Compose.

## O que garante

| Falha da vida real | Como o sistema responde |
|---|---|
| Cliente dá timeout e repete o POST | `Idempotency-Key`: o retry devolve a resposta original, sem cobrar de novo |
| Duas requisições iguais chegam juntas | Um `INSERT ... ON CONFLICT` decide o vencedor; as outras esperam ou recebem 409 |
| O processo cai depois de cobrar no PSP | Recovery points na chave: o retry retoma de onde parou; o PSP também é idempotente |
| O PSP não responde | Estado `unknown` (nunca se assume "falhou"); o reconciliador consulta o PSP e resolve |
| Captura e cancelamento ao mesmo tempo | Lock otimista por versão: exatamente um vence |
| Estornos simultâneos | Serializados pelo lock otimista; a soma nunca passa do capturado |
| Dinheiro "some" ou "aparece" | Ledger de partida dobrada, append-only, balanceado por trigger no banco |
| O evento se perde entre o commit e o envio | Outbox transacional: o evento é gravado na mesma transação da mudança |
| O webhook do lojista está fora do ar | Retry com backoff exponencial e jitter, dead letter e reenvio manual |
| Um lojista aponta o webhook para a rede interna | Proteção contra SSRF no momento da conexão |

## Arquitetura

```
cmd/server        API HTTP (8080) + porta de operação (9102)
cmd/worker        entrega de webhooks + reconciliação + retenção (porta de operação 9103)
cmd/psp-simulator PSP falso com comportamentos "mágicos" por valor
cmd/webhook-sink  receptor de webhooks de referência (valida assinatura, deduplica)
cmd/loadtest      teste de carga que verifica os invariantes sob concorrência
```

```
interfaces/http  ->  usecase  ->  domain (money, payment, ledger)
                        |
                 infrastructure (postgres, psp, webhook)
```

A regra de dependência aponta para dentro: o domínio não importa banco, HTTP nem relógio.
O `usecase` declara as portas (repositórios, `psp.Gateway`) e a infraestrutura as implementa.
Toda composição acontece em `cmd/*/main.go`.

### Fluxo de uma captura

```
cliente -> POST /v1/payments/{id}/capture -> API
  1. adquire a Idempotency-Key (INSERT ... ON CONFLICT)
  2. valida a transição authorized -> captured
  3. chama o PSP (fora de transação, idempotente por "capture:<id>")
  4. UMA transação grava: pagamento + lançamentos do ledger + evento na outbox + chave finalizada
  5. responde 200 (a resposta fica guardada: o retry devolve a mesma)
worker -> lê a outbox -> POST assinado (HMAC-SHA256) no webhook do lojista
```

Se o processo cair entre 3 e 4, o retry repete o passo 3 (o PSP devolve o mesmo resultado) e conclui.
O que nunca acontece é gravar "capturado" sem o PSP ter capturado.

## Como rodar

Requisitos: Docker (com Compose) e Go para os testes.

```bash
cp .env.example .env          # ajuste se quiser
make up                       # postgres, migrations, psp, app, worker e receptor de webhook
make merchant NAME=minha-loja # imprime merchant_id e api_key (a chave aparece só desta vez)
```

```bash
KEY=<api_key>
curl -X POST localhost:8090/v1/payments -H "Authorization: Bearer $KEY" \
  -H "Idempotency-Key: pedido-1" -d '{"amount":10000,"currency":"BRL"}'
```

Receber webhooks no receptor de exemplo:

```bash
make webhook MERCHANT=<merchant_id>   # aponta o webhook para o receptor
make webhook-events                   # estado das entregas
make webhook-replay EVENT=<event_id>  # reenvia um evento dead
```

### Endpoints

Todas as rotas `/v1/*` exigem `Authorization: Bearer <api key>`. As operações que mudam estado
exigem `Idempotency-Key`.

| Método e rota | O que faz |
|---|---|
| `POST /v1/payments` | Cria e autoriza `{"amount": <centavos>, "currency": "BRL"}` |
| `GET /v1/payments/{id}` | Consulta (`processing` = desfecho no PSP ainda não confirmado) |
| `POST /v1/payments/{id}/capture` | Captura o valor autorizado |
| `POST /v1/payments/{id}/void` | Cancela uma autorização não capturada |
| `POST /v1/payments/{id}/refund` | Estorna `{"amount": <centavos>}`, total ou parcial, várias vezes |
| `GET /v1/balance` | Saldo do lojista por moeda, derivado do ledger |
| `GET /v1/statement?limit=&before=` | Extrato paginado por cursor, do mais novo ao mais antigo |

Erros seguem `{"error": {"code": "...", "message": "..."}}`. Códigos relevantes: `409 idempotency_key_in_use`
(repita), `422 idempotency_key_reuse` (mesma chave com corpo diferente), `409 invalid_state`,
`502 psp_unavailable` (repita com a mesma chave).

### Comportamentos do simulador de PSP

O valor em centavos escolhe o comportamento: `4000` recusa, `5000` demora mas aprova (o caso
perigoso), `5001` demora e perde, `5002` instável, `5003` fora do ar, `6000` a `6005` falhas na
captura, no cancelamento e no estorno.

### Webhooks

Corpo JSON com `id` (igual a `X-Gateway-Event-Id`, use para deduplicar), `type`
(`payment.created`, `payment.authorized`, `payment.failed`, `payment.captured`, `payment.voided`,
`payment.refunded`) e `data`. Assinatura em `X-Gateway-Signature: t=<unix>,v1=<hex>`, HMAC-SHA256 de
`<t>.<corpo>` com o segredo do lojista. Verifique a assinatura e rejeite timestamps antigos.
A entrega é **pelo menos uma vez**: o receptor precisa ser idempotente.

## Testes

```bash
make test               # unitários (os de banco são pulados sem TEST_DATABASE_URL)
make test-integration   # tudo, contra Postgres real (cada teste cria e descarta um banco)
make loadtest API_KEY=<api_key>   # estresse com verificação de invariantes (app no ar)
```

A suíte cobre, contra Postgres real: retry e concorrência da mesma chave, queda do processo em
cada fase, rollback conjunto de pagamento, ledger e evento, corridas de captura, cancelamento e
estorno, SKIP LOCKED na outbox, fencing de workers lentos e reconciliação. Os componentes de
concorrência e de falha parcial foram validados também por **teste de mutação manual**: cada
alteração deliberada de uma regra (inverter um `if`, tirar um `LIMIT`) precisa derrubar pelo menos
um teste.

## Operação

- `/metrics` (Prometheus), `/health` e `/ready` em portas separadas da API pública (9102 server,
  9103 worker). Não exponha essas portas à internet.
- Métricas: HTTP por rota, PSP por operação e desfecho, entregas de webhook, reconciliação,
  retenção e gauges de backlog (outbox pendente e dead, atraso da entrega mais antiga, pagamentos
  presos). Sugestões de alerta: `gateway_outbox_dead_events > 0`, `gateway_payments_stuck`
  crescendo, `gateway_backlog_scrape_ok == 0`.
- Configuração inteira por variáveis de ambiente (`.env.example` lista as principais). A
  aplicação falha cedo se uma combinação for perigosa, por exemplo um corte de reconciliação
  menor que o pior caso de uma chamada ao PSP.
- `WEBHOOK_ALLOW_PRIVATE=true` desliga a proteção contra SSRF e existe só no compose de
  desenvolvimento. Nunca em produção.

## Limitações conhecidas

- PSP é simulado; não há PCI, antifraude, multi-região nem conciliação com banco real.
- Captura ou cancelamento que o PSP executou mas cujo registro local falhou só se corrige quando o
  cliente repete a requisição com a mesma chave (o PSP é idempotente). Não há varredura para isso.
- Sem circuit breaker por destino de webhook nem tracing distribuído.
- O segredo do webhook fica em texto na tabela `merchants` (em produção: cofre ou KMS).
- Um pagamento estornado por inteiro deixa o lojista devendo a taxa (a taxa não é devolvida).
- Sem rate limiting. Entraria na borda (gateway de API) ou como middleware com Redis.
