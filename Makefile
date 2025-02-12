.PHONY: help
help:
	@echo
	@echo "Usage: make [command]"
	@echo
	@echo "Commands:"
	@echo " build                         Build the eRPC server"
	@echo
	@echo " docker-up                     Up docker services"
	@echo " docker-down                   Down docker services"
	@echo
	@echo " fmt                           Format source code"
	@echo " test                          Run unit tests"
	@echo

.PHONY: setup
setup:
	@go mod tidy

.PHONY: run
run:
	@go run ./cmd/erpc/main.go

.PHONY: build
build:
	@CGO_ENABLED=0 go build -ldflags="-w -s" -o ./bin/erpc ./cmd/erpc/main.go
	@echo executable file \"erpc\" saved in ./bin/erpc

.PHONY: test
test:
	@go clean -testcache
	@go test ./cmd/... -race -count 1 -parallel 1
	@go test $$(ls -d */ | grep -v "cmd" | awk '{print "./" $$1 "..."}') -covermode=atomic -race -count 5 -parallel 1

.PHONY: coverage
coverage:
	@go clean -testcache
	@go test -coverprofile=coverage.txt -covermode=atomic ./...
	@go tool cover -html=coverage.txt

.PHONY: up
up:
	@docker-compose up -d --force-recreate --remove-orphans

.PHONY: down
down:
	@docker-compose down

.PHONY: fmt
fmt:
	@go fmt ./...

.DEFAULT_GOAL := help