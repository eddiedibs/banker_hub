.PHONY: setup run build test lint

setup:
	go run ./cmd/quickstart

run:
	set -a && . ./.env && set +a && go run ./cmd/bankerhub

build:
	CGO_ENABLED=0 go build -o bin/bankerhub ./cmd/bankerhub

test:
	go test -race ./...

lint:
	gofmt -l . && go vet ./...
