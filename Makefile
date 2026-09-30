GO ?= go
.PHONY: build test vet integration
build:
	$(GO) build -trimpath -o bin/mail-mcp ./cmd/server
test:
	$(GO) test -race ./...
vet:
	$(GO) vet ./...
integration:
	$(GO) test -race -tags=integration ./...
