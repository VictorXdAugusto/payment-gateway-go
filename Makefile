COMPOSE ?= docker-compose

.PHONY: up down logs test test-integration vet run migrate-down merchant

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
