SHELL := /bin/bash

BINARY := terraform-provider-stackorder
GOLANGCI_LINT_VERSION ?= v2.14.0

.PHONY: all build test testacc lint fmt generate docs-check tidy

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

docs-check: generate
	@git diff --exit-code HEAD -- docs/ examples/ || { echo "docs/ or examples/ differ from the last commit: run make generate and commit the result"; exit 1; }
	@untracked=$$(git ls-files --others --exclude-standard docs/); \
	if [ -n "$$untracked" ]; then echo "docs/ has files that are not committed:"; echo "$$untracked"; exit 1; fi

tidy:
	go mod tidy
