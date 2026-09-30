COMPOSE ?= docker-compose

.PHONY: up down logs test test-integration vet run migrate-down merchant psp-help webhook webhook-events webhook-replay

up: ## Sobe tudo (banco, migrations, app)
	$(COMPOSE) up -d --build

down: ## Derruba e remove containers (mantém o volume do banco)
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f app

test:
	go test -race -count=1 ./...

test-integration: ## Testes contra Postgres real (cada teste cria e descarta um banco)
	$(COMPOSE) up -d postgres
	TEST_DATABASE_URL="postgres://$$(grep POSTGRES_USER .env | cut -d= -f2):$$(grep POSTGRES_PASSWORD .env | cut -d= -f2)@localhost:$$(grep POSTGRES_HOST_PORT .env | cut -d= -f2)/$$(grep POSTGRES_DB .env | cut -d= -f2)?sslmode=disable" \
		go test -race -count=1 ./...

vet:
	go vet ./...

run: ## App fora do Docker, usando o banco do compose
	set -a && . ./.env && set +a && HTTP_PORT=8080 go run ./cmd/server

migrate-down: ## Desfaz a última migration
	$(COMPOSE) run --rm migrate -path=/migrations -database="postgres://$$(grep POSTGRES_USER .env | cut -d= -f2):$$(grep POSTGRES_PASSWORD .env | cut -d= -f2)@postgres:5432/$$(grep POSTGRES_DB .env | cut -d= -f2)?sslmode=disable" down 1

merchant: ## Cria um lojista e imprime a API key (aparece só esta vez): make merchant NAME=loja
	@test -n "$(NAME)" || (echo "uso: make merchant NAME=<nome>" >&2; exit 1)
	@key="sk_test_$$(openssl rand -hex 24)"; \
	echo "INSERT INTO merchants (name, api_key_hash) VALUES (:'name', encode(sha256(convert_to(:'key', 'UTF8')), 'hex')) RETURNING id;" \
	  | $(COMPOSE) exec -T postgres psql -q -tA -U $$(grep POSTGRES_USER .env | cut -d= -f2) -d $$(grep POSTGRES_DB .env | cut -d= -f2) -v name="$(NAME)" -v key="$$key" \
	  | sed 's/^/merchant_id: /'; \
	echo "api_key:     $$key"

psp-help: ## Valores (centavos) que fazem o PSP simulado se comportar de cada jeito
	@echo "4000 recusa | 5000 lento mas APROVA (fica processing) | 5001 lento e perdido | 5002 instável (retry resolve)"
	@echo "5003 fora do ar | 6000 captura instável (retry resolve) | 6001 captura recusada | 6002 captura fora do ar"

webhook: ## Aponta o webhook de um lojista: make webhook MERCHANT=<id> [URL=http://webhook-sink:9100/hook]
	@test -n "$(MERCHANT)" || (echo "uso: make webhook MERCHANT=<id do lojista>" >&2; exit 1)
	@echo "UPDATE merchants SET webhook_url = :'url', webhook_secret = :'secret' WHERE id = :'id'::uuid;" \
	  | $(COMPOSE) exec -T postgres psql -q -U $$(grep POSTGRES_USER .env | cut -d= -f2) -d $$(grep POSTGRES_DB .env | cut -d= -f2) \
	    -v url="$(or $(URL),http://webhook-sink:9100/hook)" -v secret="$$(grep SINK_SECRET .env | cut -d= -f2)" -v id="$(MERCHANT)"
	@echo "webhook configurado (segredo = SINK_SECRET do .env)"

webhook-events: ## Estado das entregas de webhook
	@echo "select event_id, type, status, attempts, left(last_error, 40) as last_error from outbox_events order by id;" \
	  | $(COMPOSE) exec -T postgres psql -U $$(grep POSTGRES_USER .env | cut -d= -f2) -d $$(grep POSTGRES_DB .env | cut -d= -f2)

webhook-replay: ## Reenvia um evento (por exemplo, um dead): make webhook-replay EVENT=<event_id>
	@test -n "$(EVENT)" || (echo "uso: make webhook-replay EVENT=<event_id>" >&2; exit 1)
	@echo "UPDATE outbox_events SET status = 'pending', attempts = 0, next_attempt_at = now(), locked_until = NULL, last_error = '' WHERE event_id = :'e' AND status IN ('dead','skipped');" \
	  | $(COMPOSE) exec -T postgres psql -q -U $$(grep POSTGRES_USER .env | cut -d= -f2) -d $$(grep POSTGRES_DB .env | cut -d= -f2) -v e="$(EVENT)"
