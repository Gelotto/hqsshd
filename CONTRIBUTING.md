# Contributing to hqsshd

Thank you for your interest in contributing to hqsshd! This document covers development setup, testing, and pull request guidelines.

## Development Setup

### Prerequisites

- Go 1.25+
- `protoc` (Protocol Buffers compiler)
- `make`

### Building

```bash
cd hqsshd

# Install protoc Go plugins
make deps

# Generate gRPC code from proto definitions
make proto

# Build binaries (hqsshd daemon + hqssh CLI)
make build
```

### Running Locally

```bash
# Run the daemon (listens on /tmp/hqssh.sock + TCP 50051)
make run

# In another terminal, verify it's running
grpcurl -plaintext unix:///tmp/hqssh.sock list
```

### Installing

```bash
# Install to /usr/local/bin (requires sudo)
make install

# Or install to ~/.local/bin (no sudo)
make install-user
```

## Testing

```bash
# Run all tests
go test ./...

# Run tests with race detector
go test -race ./...

# Run vet
go vet ./...
```

## Pull Request Guidelines

1. **Fork & branch**: Create a feature branch from `main` (e.g., `fix/session-cleanup`, `feat/cron-tasks`)
2. **Keep it focused**: One logical change per PR
3. **Test**: Ensure `go test -race ./...` and `go vet ./...` pass
4. **Build**: Ensure `make build` succeeds
5. **Describe**: Write a clear PR description explaining _what_ and _why_

### Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Use structured logging via `internal/logging` (not `fmt.Printf` for operational messages)
- Add license headers to new `.go` files (see existing files for the format)
- Keep error messages lowercase (Go convention)

### Commit Messages

- Use conventional style: `fix:`, `feat:`, `chore:`, `docs:`
- Keep the first line under 72 characters
- Reference issues where applicable (e.g., `Fixes #42`)

## Project Structure

```
hqsshd/
├── cmd/hqsshd/          # Daemon entry point
├── cmd/hqssh/           # CLI entry point
├── proto/               # gRPC proto definitions + generated code
└── internal/
    ├── config/           # YAML configuration
    ├── logging/          # Structured logging (slog)
    ├── server/           # gRPC server + service implementations
    ├── session/          # PTY session management
    ├── project/          # Project discovery + registry
    ├── task/             # Task execution + persistence
    ├── tools/            # AI tool detection
    └── cli/              # Desktop CLI client
```

## Questions?

Open a [discussion](https://github.com/Gelotto/hqsshd/discussions) or file an issue.
