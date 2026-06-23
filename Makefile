.PHONY: build test lint lint-fix

BINARY_NAME=gopls-lazy

build:
	go build -o $(BINARY_NAME) ./cmd/$(BINARY_NAME)

test:
	go test -race ./...

lint:
	golangci-lint run ./...

lint-fix:
	golangci-lint run --fix ./...
