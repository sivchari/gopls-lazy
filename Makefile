.PHONY: build test test-e2e lint lint-fix

BINARY_NAME=gopls-lazy

build:
	go build -o $(BINARY_NAME) ./cmd/$(BINARY_NAME)

test:
	go test -race -short ./...

# test-e2e drives a real gopls through the proxy (TestE2E).
# Requires gopls and go in PATH: go install golang.org/x/tools/gopls@latest
test-e2e:
	go test -race -run TestE2E -timeout 20m ./...

lint:
	golangci-lint run ./...

lint-fix:
	golangci-lint run --fix ./...
