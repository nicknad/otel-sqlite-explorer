# Log Explorer — development targets
# Recipes are kept shell-agnostic (no sh-only syntax) so `make` works with
# both cmd.exe on Windows and sh on Unix.

BIN     := bin/log-explorer
MOCK_DB := demo/logs.db
ADDR    := :8080

# `rm` is not available on Windows, and `del` rejects forward-slash paths, so
# the delete command and its arguments are picked per OS.
ifeq ($(OS),Windows_NT)
  RM        := del /q
  CLEANARGS := $(subst /,\,$(BIN) $(MOCK_DB))
else
  RM        := rm -f
  CLEANARGS := $(BIN) $(MOCK_DB)
endif

.PHONY: all build fmt test lint vet mock-db serve-mock run-mock clean-mock

all: build

build:
	-mkdir bin
	go build -o $(BIN) ./cmd/server

fmt:
	go fmt ./...

test:
	go test ./...

lint:
	golangci-lint run

vet:
	go vet ./...

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

clean-mock:
	-$(RM) $(CLEANARGS)
