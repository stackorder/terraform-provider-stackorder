SHELL := /bin/bash

BINARY := terraform-provider-stackorder
GOLANGCI_LINT_VERSION ?= v2.14.0

.PHONY: all build test testacc lint fmt generate tidy

all: build

build:
	go build -o $(BINARY)

test:
	go test -race -count=1 ./...

testacc:
	TF_ACC=1 go test -count=1 -run 'TestAcc' ./internal/provider/... -timeout 20m

lint:
	@out=$$(gofmt -s -l .) || exit 1; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

fmt:
	gofmt -s -w .

generate:
	go generate ./...

tidy:
	go mod tidy
