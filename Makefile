# Log Explorer — development targets
# Recipes are kept shell-agnostic (no sh-only syntax) so `make` works with
# both cmd.exe on Windows and sh on Unix.

BIN     := bin/log-explorer
MOCK_DB := demo/logs.db
SINK_DB := demo/sink-logs.db
ADDR    := :8080

# `rm` is not available on Windows, and `del` rejects forward-slash paths, so
# the delete command and its arguments are picked per OS.
ifeq ($(OS),Windows_NT)
  RM      := del /q
  DELBIN  := $(subst /,\,$(BIN))
  DELMOCK := $(subst /,\,$(MOCK_DB))
  DELSINK := $(subst /,\,$(SINK_DB))
else
  RM      := rm -f
  DELBIN  := $(BIN)
  DELMOCK := $(MOCK_DB)
  DELSINK := $(SINK_DB)
endif

.PHONY: all build fmt test lint vet mock-db serve-mock run-mock clean clean-mock test-image sink-demo serve-sink-demo clean-sink

all: build

build:
	-mkdir bin
	go build -o $(BIN) ./cmd/server

fmt:
	go fmt ./...

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run

vet:
	go vet ./...

# Image-backed integration tests: the read contract against a database
# produced by the real otel-sqlite sink image (ghcr.io/nicknad/otel-sqlite).
# Requires a Docker daemon; the tests skip themselves when none is reachable.
# Pin or force an image with OTEL_SQLITE_IMAGE=<ref>.
test-image:
	go test -count=1 -v -run '^TestSinkImage' ./internal/tests

# Harvest a fresh sink-produced database for collectorDDL regeneration.
# Requires a Docker daemon; the dump lands in .fixtures/sink-harvest.db.
# Regenerate the collectorDDL constant in internal/tests from that database's
# schema and diff (docs/CONTRACT.md, docs/COUPLING-AUDIT.md section 2).
collector-ddl:
	mkdir -p .fixtures
	EXPLORER_SINK_HARVEST_DUMP=.fixtures go test -count=1 -run '^TestSinkImage' ./internal/tests

# Generate the demo database used to inspect the UI with realistic data.
mock-db:
	go run ./cmd/mockdb -out $(MOCK_DB)

# Start the site against the demo database. The database is regenerated each
# run so timestamps are always recent and the dataset matches the current
# generator output.
serve-mock: mock-db
	go run ./cmd/server -db $(MOCK_DB) -addr $(ADDR)

# Build the server binary and start it against the demo database.
run-mock: build mock-db
	$(BIN) -db $(MOCK_DB) -addr $(ADDR)

# Generate the sink-backed demo database: real OTLP traffic pushed through
# the published otel-sqlite sink image and harvested, the same ingestion
# path production uses. Requires a Docker daemon; OTEL_SQLITE_IMAGE=<ref>
# pins a different sink image.
sink-demo:
	go run ./cmd/sinkdemo -out $(SINK_DB)

# Start the site against the sink-backed demo database (regenerated each
# run so timestamps stay recent).
serve-sink-demo: sink-demo
	go run ./cmd/server -db $(SINK_DB) -addr $(ADDR)

clean:
	-$(RM) $(DELBIN)

clean-mock:
	-$(RM) $(DELMOCK)

clean-sink:
	-$(RM) $(DELSINK)
