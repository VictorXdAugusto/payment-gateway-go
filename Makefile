COMPOSE ?= docker-compose

.PHONY: up down logs test test-integration vet run migrate-down

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
