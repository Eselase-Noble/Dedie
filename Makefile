# Dedie build targets. `make` builds the web interface, the server and the CLI;
# `make desktop` builds the JavaFX desktop application.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFLAGS := -trimpath -ldflags "-s -w"
BIN     := bin
# Windows binaries need the .exe suffix when cross-compiling with GOOS=windows.
EXT     := $(if $(filter windows,$(GOOS)),.exe,)

.PHONY: all web build desktop test test-integration lint clean

all: web build

# Static export of the web interface, embedded into the binaries by `build`.
web:
	cd web && npm ci && npm run build

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -o $(BIN)/dedie$(EXT) ./apps/cli
	CGO_ENABLED=0 go build $(GOFLAGS) -o $(BIN)/dedie-server$(EXT) ./apps/server

# The desktop application (JDK 17+ and Maven). It is assembled in
# bin/desktop together with the server it runs: start it with
# `java -jar bin/desktop/dedie-desktop.jar`.
desktop: build
	cd apps/desktop && mvn -q package
	rm -rf $(BIN)/desktop && mkdir -p $(BIN)/desktop
	cp -r apps/desktop/target/dedie-desktop.jar apps/desktop/target/lib $(BIN)/desktop/
	cp $(BIN)/dedie-server$(EXT) $(BIN)/desktop/

# Unit tests need neither Node nor a database.
test:
	go test -tags noui ./...

# End-to-end migration tests. Each needs two database servers and skips
# itself when its variables are not set; see the integration_test.go file of
# each pipeline under engine/migrate.
test-integration:
	go test -tags noui -count=1 -v -run TestMigration ./engine/migrate/...

lint:
	gofmt -l apps engine web/*.go | (! grep .)
	go vet -tags noui ./...

clean:
	rm -rf $(BIN) web/out web/.next apps/desktop/target
