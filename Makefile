.PHONY: build proto install install-user clean deps run tidy test test-race test-coverage test-verbose test-short

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOMOD=$(GOCMD) mod

# Binary names
DAEMON_BINARY=hqsshd
CLI_BINARY=hqssh

# Build directory
BUILD_DIR=bin

# The binary targets are phony so an existing bin/ artifact never suppresses
# a rebuild (make has no Go dependency tracking). Must come after the
# variable definitions above — .PHONY expands its arguments immediately.
.PHONY: $(BUILD_DIR)/$(DAEMON_BINARY) $(BUILD_DIR)/$(CLI_BINARY)

# Version injection
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
LDFLAGS=-ldflags "-X github.com/gelotto/hqsshd/internal/config.DaemonVersion=$(VERSION) -X github.com/gelotto/hqsshd/internal/config.Commit=$(COMMIT)"

# Build both binaries
build: $(BUILD_DIR)/$(DAEMON_BINARY) $(BUILD_DIR)/$(CLI_BINARY)

$(BUILD_DIR)/$(DAEMON_BINARY):
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) -trimpath $(LDFLAGS) -o $(BUILD_DIR)/$(DAEMON_BINARY) ./cmd/hqsshd

$(BUILD_DIR)/$(CLI_BINARY):
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) -trimpath $(LDFLAGS) -o $(BUILD_DIR)/$(CLI_BINARY) ./cmd/hqssh

# Generate protobuf code
proto:
	protoc --go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		proto/hqssh.proto

# Install protoc plugins (run once)
deps:
	$(GOCMD) install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	$(GOCMD) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

# Install to /usr/local/bin (requires sudo)
install: build
	sudo cp $(BUILD_DIR)/$(DAEMON_BINARY) /usr/local/bin/
	sudo cp $(BUILD_DIR)/$(CLI_BINARY) /usr/local/bin/

# Install to ~/.local/bin (no sudo required)
install-user: build
	@mkdir -p ~/.local/bin
	cp $(BUILD_DIR)/$(DAEMON_BINARY) ~/.local/bin/
	cp $(BUILD_DIR)/$(CLI_BINARY) ~/.local/bin/
	@echo "Installed to ~/.local/bin/"
	@echo "Make sure ~/.local/bin is in your PATH"

# Run the daemon
run: build
	./$(BUILD_DIR)/$(DAEMON_BINARY)

# Clean build artifacts
clean:
	$(GOCLEAN)
	rm -rf $(BUILD_DIR)

# Download dependencies
tidy:
	$(GOMOD) tidy

# Run tests
test:
	$(GOCMD) test ./...

# Run tests with race detector
test-race:
	$(GOCMD) test -race ./...

# Run tests with coverage
test-coverage:
	$(GOCMD) test -cover ./...

# Run tests with verbose output
test-verbose:
	$(GOCMD) test -v ./...

# Run short tests only (skip integration tests)
test-short:
	$(GOCMD) test -short ./...
