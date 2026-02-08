# hqsshd

A daemon for managing persistent PTY sessions for AI CLI tools (Claude Code, Codex, Aider) across remote systems.

## Overview

hqsshd provides session persistence and multi-client access for AI coding assistants running on remote servers. Start a Claude Code session on your server, detach, and reattach from any device - your session state is preserved.

**Key Features:**

- **Persistent Sessions** - AI tool sessions survive disconnects and continue running
- **Multi-Client Attach** - Multiple clients can attach to the same session simultaneously
- **Scrollback Buffer** - New clients receive session history on attach
- **Project Discovery** - Automatically discovers git repositories
- **Task Automation** - Define and run automated tasks with timeout support
- **Secure by Design** - Listens only on localhost; access via SSH tunnel

## Architecture

```
┌─────────────────┐         ┌─────────────────┐
│  HQSSH Mobile   │──SSH───▶│    hqsshd       │
│  (iOS/Android)  │ tunnel  │  (this daemon)  │
└─────────────────┘         └────────┬────────┘
                                     │
┌─────────────────┐                  │ manages
│  hqssh CLI      │──SSH────────────▶│
│  (desktop)      │ tunnel           ▼
└─────────────────┘         ┌─────────────────┐
                            │  PTY Sessions   │
                            │  claude, codex, │
                            │  aider, shell   │
                            └─────────────────┘
```

## Installation

### Prebuilt Binaries

Download the latest release for your platform from [GitHub Releases](https://github.com/gelotto/hqsshd/releases/latest):

```bash
# Linux (amd64)
curl -LO https://github.com/gelotto/hqsshd/releases/latest/download/hqsshd-linux-amd64.tar.gz
tar xzf hqsshd-linux-amd64.tar.gz
sudo mv hqsshd hqssh /usr/local/bin/

# Linux (arm64)
curl -LO https://github.com/gelotto/hqsshd/releases/latest/download/hqsshd-linux-arm64.tar.gz
tar xzf hqsshd-linux-arm64.tar.gz
sudo mv hqsshd hqssh /usr/local/bin/

# macOS (Apple Silicon)
curl -LO https://github.com/gelotto/hqsshd/releases/latest/download/hqsshd-darwin-arm64.tar.gz
tar xzf hqsshd-darwin-arm64.tar.gz
sudo mv hqsshd hqssh /usr/local/bin/

# macOS (Intel)
curl -LO https://github.com/gelotto/hqsshd/releases/latest/download/hqsshd-darwin-amd64.tar.gz
tar xzf hqsshd-darwin-amd64.tar.gz
sudo mv hqsshd hqssh /usr/local/bin/
```

Verify:
```bash
hqsshd --version
```

### From Source

Requires Go 1.24+:

```bash
git clone https://github.com/gelotto/hqsshd.git
cd hqsshd
make build
make install-user  # Installs to ~/.local/bin
```

### Systemd Service (Recommended)

Install as a user service for auto-start:

```bash
# Copy service file
mkdir -p ~/.config/systemd/user
cp hqsshd.service ~/.config/systemd/user/

# Enable and start
systemctl --user daemon-reload
systemctl --user enable hqsshd
systemctl --user start hqsshd

# View logs
journalctl --user -u hqsshd -f
```

## Usage

### Daemon

```bash
# Run daemon (foreground)
hqsshd

# With custom config
hqsshd --config /path/to/config.yaml

# Check version
hqsshd --version
```

### CLI (hqssh)

The CLI connects to the daemon via SSH tunnel:

```bash
# List sessions on remote host
hqssh sessions -H myserver.com -u myuser

# Attach to a session
hqssh attach <session-id> -H myserver.com -u myuser

# Detach from session: Ctrl+B, then D
```

**SSH Authentication:**
- Uses your default SSH keys (`~/.ssh/id_ed25519`, `~/.ssh/id_rsa`)
- Verifies host keys against `~/.ssh/known_hosts`
- Use `--insecure` to skip host key verification (not recommended)

## Configuration

Default config location: `~/.hqssh/daemon.yaml`

```yaml
# Session settings
session:
  idle_timeout_sec: 3600    # Kill idle sessions after 1 hour (0 = disabled)
  max_sessions: 10          # Maximum concurrent sessions
  history_lines: 10000      # Scrollback buffer size

# Server settings
server:
  socket_path: /tmp/hqssh.sock  # Unix socket for local tools
  tcp_port: 50051               # TCP port for SSH tunnel access
```

## Security

hqsshd is designed with security in mind:

- **Local-only by default** - TCP server binds to `127.0.0.1`, accessible only via SSH tunnel
- **No authentication layer** - Relies on SSH for authentication and encryption
- **Minimal privileges** - Runs as your user, no root required
- **Open source** - Full source code available for audit

**What hqsshd does:**
- Spawns PTY processes for AI tools (claude, codex, aider, shell)
- Manages session lifecycle (create, attach, detach, kill)
- Discovers git repositories in specified directories
- Executes user-defined tasks with configurable timeouts

**What hqsshd does NOT do:**
- Phone home or collect telemetry
- Store credentials or secrets
- Modify system configuration
- Run with elevated privileges

## gRPC API

hqsshd exposes a gRPC API for programmatic access:

| Service | Description |
|---------|-------------|
| `SystemService` | System info and health status |
| `ProjectService` | Git repository discovery and management |
| `SessionService` | PTY session lifecycle management |
| `TaskService` | Automated task execution |

Proto definitions: [`proto/hqssh.proto`](proto/hqssh.proto)

## Building

```bash
# Install dependencies
make deps

# Regenerate proto (if modified)
make proto

# Build binaries
make build

# Run tests
make test

# Clean build artifacts
make clean
```

## Data Storage

All data is stored in `~/.hqssh/`:

| File | Purpose |
|------|---------|
| `daemon.yaml` | Configuration (optional) |
| `projects.json` | Registered projects |
| `tasks.json` | Task definitions |
| `runs/` | Task execution history |

## License

Apache License 2.0 - See [LICENSE](LICENSE)

## Contributing

Contributions welcome! Please open an issue to discuss significant changes before submitting a PR.
