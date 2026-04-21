.PHONY: proto build up down logs clean

GOPATH := $(shell go env GOPATH)
PATH := $(PATH):$(GOPATH)/bin

proto:
	protoc --go_out=replica/proto --go_opt=paths=source_relative \
	       --go-grpc_out=replica/proto --go-grpc_opt=paths=source_relative \
	       -I=replica/proto replica/proto/raft.proto
	@echo "Proto stubs generated"

build:
	docker compose build

up:
	docker compose up --build

down:
	docker compose down

logs:
	docker compose logs -f

clean:
	docker compose down -v
	rm -rf logs/replica1/* logs/replica2/* logs/replica3/* logs/replica4/* logs/gateway/*
	find . -name "tmp" -type d -exec rm -rf {} + 2>/dev/null || true

test-replica:
	cd replica && go test ./...

test-gateway:
	cd gateway && go test ./...
