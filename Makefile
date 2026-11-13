.PHONY: build test lint run up down fmt cover

BIN      := bin/llmgate
IMAGE    ?= localhost/llmgate:dev
CONFIG   ?= config.example.yaml
COMPOSE  ?= $(shell if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then echo "docker compose"; else echo "podman compose"; fi)
COMPOSE_FILE := deploy/docker-compose.yml

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/llmgate

test:
	go test -race -cover ./...

cover:
	go test -race -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

fmt:
	gofmt -w .

lint:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	go vet ./...
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; \
	else echo "staticcheck not installed: go install honnef.co/go/tools/cmd/staticcheck@latest"; fi

# Runs against a local Redis (redis://localhost:6379). Mock providers need no keys.
run:
	LLMGATE_CONFIG=$(CONFIG) LLMGATE_DEMO_KEY=$${LLMGATE_DEMO_KEY:-demo-key} LLMGATE_ADMIN=1 go run ./cmd/llmgate

up:
	$(COMPOSE) -p llmgate -f $(COMPOSE_FILE) up -d --build

down:
	$(COMPOSE) -p llmgate -f $(COMPOSE_FILE) down
