# syntax=docker/dockerfile:1

# ---- Build ----
FROM golang:1.27-alpine AS builder
WORKDIR /src

# go.mod/go.sum antes do código: a camada de dependências só é refeita se elas mudarem.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/psp-simulator ./cmd/psp-simulator \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/webhook-sink ./cmd/webhook-sink

# ---- Runtime ----
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S app && adduser -S -G app app
COPY --from=builder /out/server /out/psp-simulator /out/worker /out/webhook-sink /usr/local/bin/
USER app
EXPOSE 8080
ENTRYPOINT ["server"]
