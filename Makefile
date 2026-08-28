.PHONY: build test test-e2e lint lint-fix bench

BINARY_NAME=golance

build:
	go build -o $(BINARY_NAME) ./cmd/$(BINARY_NAME)

test:
	go test -race -short ./...

test-e2e:
	go test -race -run TestE2E -timeout 20m ./...

bench:
	go test -bench=. -benchmem -run=^$$ ./internal/store/...

lint:
	golangci-lint run ./...

lint-fix:
	golangci-lint run --fix ./...
