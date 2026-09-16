RENAME_MODULE_FROM=github.com/glennprays/catet-backend
RENAME_MODULE_TO=

DATABASE_URL ?= postgres://postgres:catet_dev@localhost:5432/catet_dev?sslmode=disable

# detect OS
UNAME_S := $(shell uname -s)

# Build metadata baked in via -ldflags. Override with VERSION=x.y.z make build.
VERSION    ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w \
  -X github.com/glennprays/catet-backend/pkg/buildinfo.Version=$(VERSION) \
  -X github.com/glennprays/catet-backend/pkg/buildinfo.Commit=$(COMMIT) \
  -X github.com/glennprays/catet-backend/pkg/buildinfo.BuildTime=$(BUILD_TIME)

rename:
	@if [ -z "$(RENAME_MODULE_TO)" ]; then \
		echo "Please provide RENAME_MODULE_TO, e.g., make rename RENAME_MODULE_TO=github.com/yourname/yourproject"; \
		exit 1; \
	fi
	@if [ "$(UNAME_S)" = "Darwin" ]; then \
		sed -i '' "s|$(RENAME_MODULE_FROM)|$(RENAME_MODULE_TO)|g" go.mod; \
		find . -type f -name '*.go' -exec sed -i '' "s|$(RENAME_MODULE_FROM)|$(RENAME_MODULE_TO)|g" {} +; \
	else \
		sed -i "s|$(RENAME_MODULE_FROM)|$(RENAME_MODULE_TO)|g" go.mod; \
		find . -type f -name '*.go' -exec sed -i "s|$(RENAME_MODULE_FROM)|$(RENAME_MODULE_TO)|g" {} +; \
	fi
	go mod tidy

run-dev:
	@echo "Starting all Docker containers for development mode..."
	@sh docker-dev up -d

stop-dev:
	@echo "Stopping all Docker containers..."
	@sh docker-dev down

migrate:
	@echo "Applying migrations to $(DATABASE_URL)..."
	@migrate -path ./migrations -database "$(DATABASE_URL)" up
migrate-down:
	@echo "Rolling back last migration..."
	@migrate -path ./migrations -database "$(DATABASE_URL)" down 1

swagger:
	@echo "Restarting Swagger UI..."
	@sh docker-dev restart swagger-ui

generate:
	@echo "Generating Wire dependencies..."
	@wire gen ./internal/infrastructure/...

run:
	@go run cmd/api/main.go

dev:
	@air -c .air.toml

build:
	@go build -ldflags "$(LDFLAGS)" -o ./tmp/main ./cmd/api

# Operator CLI (account break-glass): ./tmp/catet user list
cli:
	@go build -o ./tmp/catet ./cmd/catet

hooks:
	@lefthook install

# Integration tests truncate their schema. They run against
# TEST_DATABASE_URL (a *_test database, created on demand) and the
# harness refuses anything else, so dev data is never at risk.
test:
	@go test ./... -race -count=1

# Rebuilds the development dataset from scratch (destructive, dev only).
seed:
	@echo "Seeding development data into $(DATABASE_URL)..."
	@DATABASE_URL="$(DATABASE_URL)" go run ./cmd/seed

lint:
	@golangci-lint run ./...

tidy:
	@go mod tidy
