# dedie build targets. `make` builds the web interface and all binaries.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFLAGS := -trimpath -ldflags "-s -w"
BIN     := bin
# Windows binaries need the .exe suffix when cross-compiling with GOOS=windows.
EXT     := $(if $(filter windows,$(GOOS)),.exe,)

.PHONY: all web build test test-integration lint clean

all: web build

# Static export of the web interface, embedded into the binaries by `build`.
web:
	cd web && npm ci && npm run build

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -o $(BIN)/dedie$(EXT) ./apps/cli
	CGO_ENABLED=0 go build $(GOFLAGS) -o $(BIN)/dedie-server$(EXT) ./apps/server
	CGO_ENABLED=0 go build $(GOFLAGS) -o $(BIN)/dedie-desktop$(EXT) ./apps/desktop

# Unit tests need neither Node nor a database.
test:
	go test -tags noui ./...

# End-to-end migration test. Needs two PostgreSQL servers; see
# engine/migrate/pgpg/integration_test.go for the two variables to set.
test-integration:
	go test -tags noui -count=1 -v -run TestMigration ./engine/migrate/pgpg/

lint:
	gofmt -l apps engine web/*.go | (! grep .)
	go vet -tags noui ./...

clean:
	rm -rf $(BIN) web/out web/.next
