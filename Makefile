VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w \
	-X github.com/jin-k8s/jin/internal/buildinfo.Version=$(VERSION) \
	-X github.com/jin-k8s/jin/internal/buildinfo.Commit=$(COMMIT)

.PHONY: build build-go ui ui-dev test lint fmt e2e clean

# Full build: web UI embedded into the Go binary.
build: ui build-go

build-go:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/jin ./cmd/jin

ui:
	cd web && npm ci --ignore-scripts && npm run build
	touch internal/server/ui/dist/.keep

# Vite dev server with hot reload; proxies /api to a running `jin server` on :7420.
ui-dev:
	cd web && npm run dev

test:
	go test -race ./...
	cd web && npm run typecheck

lint:
	golangci-lint run ./...

fmt:
	golangci-lint fmt ./...

e2e: build
	./hack/e2e.sh

clean:
	rm -rf bin
	find internal/server/ui/dist -mindepth 1 ! -name .keep -delete
