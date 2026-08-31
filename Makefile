.PHONY: build test test-e2e lint lint-fix lint-local bench

BINARY_NAME=golance

# Bump when go.mod's `go` directive changes. Used only by lint-local, which
# needs a stock Go SDK for `go/packages` loading (see lint-local below).
LINT_GO_VERSION := 1.26.4
# The toolchain the go command downloads for a `toolchain`/`go` directive is a
# complete SDK, so prefer it: it is already present after any build that
# resolved this version, and needs no network or write access outside the
# module cache. golang.org/dl (~/sdk) is the fallback when it is absent.
LINT_TOOLCHAIN_GOROOT := $(shell go env GOMODCACHE)/golang.org/toolchain@v0.0.1-go$(LINT_GO_VERSION).$(shell go env GOOS)-$(shell go env GOARCH)
LINT_DL_GOROOT := $(HOME)/sdk/go$(LINT_GO_VERSION)

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

# lint-local runs golangci-lint the same way CI does (golangci-lint-action
# with version: latest against go.mod's go directive), but forces a stock
# go$(LINT_GO_VERSION) toolchain for the go/packages loader.
#
# Why: golangci-lint shells out to `go list -json` to load packages. If your
# `go` in PATH is a non-release build (e.g. a `go1.27-devel` custom build),
# that call can fail with "no go files to analyze" even though the code is
# fine. Pointing GOROOT/PATH at a real release SDK fixes it.
lint-local:
	@goroot="$(LINT_TOOLCHAIN_GOROOT)"; \
	if [ ! -x "$$goroot/bin/go" ]; then \
		goroot="$(LINT_DL_GOROOT)"; \
		if [ ! -x "$$goroot/bin/go" ]; then \
			echo "installing go$(LINT_GO_VERSION) SDK to $$goroot..."; \
			go install golang.org/dl/go$(LINT_GO_VERSION)@latest; \
			bindir="$$(go env GOBIN)"; \
			[ -n "$$bindir" ] || bindir="$$(go env GOPATH)/bin"; \
			"$$bindir/go$(LINT_GO_VERSION)" download; \
		fi; \
	fi; \
	echo "linting with GOROOT=$$goroot"; \
	GOROOT="$$goroot" PATH="$$goroot/bin:$$PATH" golangci-lint run ./...
