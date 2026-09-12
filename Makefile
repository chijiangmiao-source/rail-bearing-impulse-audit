.PHONY: build test vet fmt run verify verify-health docker-build docker-up docker-verify

BIN_DIR := bin

build:
	go build -buildvcs=false -o $(BIN_DIR)/api ./cmd/api
	go build -buildvcs=false -o $(BIN_DIR)/verify ./cmd/verify

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

run: build
	PORT=8080 ./$(BIN_DIR)/api

# Acceptance suite against an already-running local API.
verify: build
	VERIFY_BASE_URL=http://127.0.0.1:8080 ./$(BIN_DIR)/verify

docker-build:
	docker compose build

docker-up:
	docker compose up --build -d

docker-verify:
	docker compose run --rm verify
