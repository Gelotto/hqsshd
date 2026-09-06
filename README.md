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

This downloads the latest release, verifies the checksum, installs both binaries (`hqsshd` + `hqssh`) into `~/.local/bin`, sets up a background service — a systemd user service on Linux, a launchd user agent on macOS — starts it, and verifies it answers. Re-run to update; the running daemon is stopped cleanly first and the service definition is refreshed.

On macOS the service starts when you log in to the Mac, so after a reboot log in at the Mac once and it comes up on its own.

Other options:

```bash
curl -fsSL https://hqssh.com/install | HQSSH_VERSION=v1.4.0 sh          # pin a version
curl -fsSL https://hqssh.com/install | sh -s -- --uninstall              # remove (keeps ~/.hqssh data)
curl -fsSL https://hqssh.com/install | sh -s -- --uninstall --purge-logs
```

The installer exits `0` on success, `1` when nothing was changed, and `3` when the binaries were installed but the service did not come up — in which case it prints the service manager's state, the last log lines, and the next commands to run.

## Manual Installation

### Prebuilt Binaries

Download the latest release for your platform from [GitHub Releases](https://github.com/gelotto/hqsshd/releases/latest):

```bash
# Linux (amd64)
curl -LO https://github.com/gelotto/hqsshd/releases/latest/download/hqsshd-linux-amd64.tar.gz
tar xzf hqsshd-linux-amd64.tar.gz
mv hqsshd hqssh ~/.local/bin/

# Linux (arm64)
curl -LO https://github.com/gelotto/hqsshd/releases/latest/download/hqsshd-linux-arm64.tar.gz
tar xzf hqsshd-linux-arm64.tar.gz
mv hqsshd hqssh ~/.local/bin/

# macOS (Apple Silicon)
curl -LO https://github.com/gelotto/hqsshd/releases/latest/download/hqsshd-darwin-arm64.tar.gz
tar xzf hqsshd-darwin-arm64.tar.gz
mv hqsshd hqssh ~/.local/bin/

# macOS (Intel)
curl -LO https://github.com/gelotto/hqsshd/releases/latest/download/hqsshd-darwin-amd64.tar.gz
tar xzf hqsshd-darwin-amd64.tar.gz
mv hqsshd hqssh ~/.local/bin/
```

Verify:
```bash
hqsshd --version
hqssh --version
```

A manual install has no background service. Re-running the installer with the binaries already in place writes and loads one, or use the service files below.

### From Source

Requires Go 1.25+:

```bash
git clone https://github.com/gelotto/hqsshd.git
cd hqsshd
make build
make install-user  # Installs to ~/.local/bin
```

To install the service after a source build, package the binaries and hand them to the installer:

```bash
tar -czf /tmp/hqsshd-$(go env GOOS)-$(go env GOARCH).tar.gz -C bin hqsshd hqssh
HQSSH_LOCAL_ARCHIVE=/tmp/hqsshd-$(go env GOOS)-$(go env GOARCH).tar.gz sh scripts/install.sh
```

## Service Management

The installer sets up a background service and `hqssh` manages it on both platforms. The raw commands are listed for reference; `hqssh service status` prints them for your installation.

| Action | `hqssh` | macOS (user agent) | Linux (systemd --user) |
|---|---|---|---|
| Health check | `hqssh doctor` | — | — |
| Status | `hqssh service status` | `launchctl print gui/$(id -u)/com.gelotto.hqsshd` | `systemctl --user status hqsshd` |
| Start | `hqssh service start` | `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.gelotto.hqsshd.plist` | `systemctl --user start hqsshd` |
| Stop | `hqssh service stop` | `launchctl bootout gui/$(id -u)/com.gelotto.hqsshd` | `systemctl --user stop hqsshd` |
| Restart | `hqssh service restart` | `launchctl kickstart -k gui/$(id -u)/com.gelotto.hqsshd` | `systemctl --user restart hqsshd` |
| Logs | `hqssh service logs -f` | `tail -f ~/.hqssh/logs/hqsshd.log` | `journalctl --user -u hqsshd -f` |

`hqssh doctor` runs without a daemon and checks the binaries, the service, the Unix socket, the TCP port the mobile app uses, configuration, logs and the data directory; every failed check prints the command that fixes it, and the exit status is `1` when anything failed (`--quiet` for scripts, `--json` for tooling).

Both service definitions restart the daemon after a crash or non-zero exit. A clean stop (`hqssh service stop`, exit 0) stays stopped until you start it again. The service manager gives the daemon 30 s to shut down (`ExitTimeOut` / `TimeoutStopSec`); its own graceful stop is bounded at about 10 s.

Restarting the daemon ends every session it hosts — the AI tools running inside them are terminated.


### Service files

The installer writes these; they are also in the repository for reference: [`hqsshd.service`](hqsshd.service) (systemd) and the plist template inside [`scripts/install.sh`](scripts/install.sh) (launchd). Both set the daemon's `HOME` and a 30 s stop timeout; the plist also sets a PATH that includes user tool directories (the daemon adds `~/.local/bin`, `~/bin`, Homebrew and `/usr/local/bin` itself at startup, so the systemd unit needs none).

## Usage

### Daemon

```bash
# Run daemon (foreground)
hqsshd

# With custom config / socket / log level
hqsshd --config /path/to/config.yaml
hqsshd --socket /tmp/other.sock
hqsshd --log-level debug

# Check version
hqsshd --version
```

A second `hqsshd` refuses to start while one is running — the running daemon holds an `flock` on `~/.hqssh/hqsshd.pid` for its lifetime, which the kernel releases when it dies, so a crash, a SIGKILL or a reboot can never leave a lock behind — and tells you how to stop the first. Exit codes:

| Code | Meaning |
|---|---|
| 0 | clean shutdown (SIGTERM / SIGINT) |
| 1 | runtime error while serving |
| 2 | configuration error (bad YAML, invalid value) |
| 3 | already running (another hqsshd holds the pidfile lock or answers on the socket) |
| 4 | bind failure (socket path unusable, or a TCP error other than "port in use") |
| 5 | log file cannot be created or opened |

The service manager restarts any non-zero exit; each attempt logs one `ERROR` line naming the cause and the fix.

A TCP port held by *another* program is not fatal: after 5 s of retries the daemon logs an `ERROR`, serves the Unix socket, keeps retrying the port every 5 s and binds it as soon as it is free — an old instance draining its shutdown resolves itself. Until then `hqssh doctor` and `hqssh service status` report the missing transport (the mobile app needs it).

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

On the machine that runs the daemon, `hqssh` needs no flags: it finds `/tmp/hqssh.sock` on its own. `hqssh version` also reports the running daemon's version.

## Configuration

Default config location: `~/.hqssh/daemon.yaml`

```yaml
# Server settings
socket: /tmp/hqssh.sock     # Unix socket for local tools (macOS caps the path at 103 bytes)
tcp_port: 50051             # TCP port for SSH tunnel access (0 = disabled; the mobile app needs it)

# Logging
log:
  level: info               # debug, info, warn, error
  format: text              # text, json
  file: ""                  # Empty = stdout (captured by launchd / journald)

# Session settings
sessions:
  idle_timeout: 86400       # Kill idle sessions after 24 hours (0 = disabled)
  max_sessions: 20          # Maximum concurrent sessions
  history_size: 10000       # Scrollback buffer size (lines)

# Push notifications for session events (bell rung, session ended).
# Point webhook_url at an https://ntfy.sh/<your-topic> URL and install the
# ntfy app to get phone push when an AI agent needs attention — even when
# the HQSSH app is closed. Works with any HTTP receiver (ntfy-compatible
# Title/Priority/Tags headers + X-HQSSH-* headers for custom relays).
events:
  webhook_url: ""           # Empty = disabled

# Debugging only: expose the gRPC schema to grpcurl (off by default)
enable_reflection: false
```

The daemon reads the file at startup; restart it after editing (`hqssh service restart`).

## Troubleshooting

Start with `hqssh doctor`. The situations below are the ones it diagnoses most often.

**The daemon is not running after a reboot (macOS).** The service starts when you log in to the Mac; until then the phone sees "daemon not running" while SSH works fine. Log in at the Mac once and it comes up. `hqssh doctor` warns when the daemon started long after boot.

**`Bootstrap failed: 5: Input/output error`** — launchd already has the job (the installer reuses it), or it rejects the plist: check `plutil -lint ~/Library/LaunchAgents/com.gelotto.hqsshd.plist` and that the binary it points to is executable. **`125: Domain does not support specified action` / `Could not find domain`** — there is no GUI session for your user (SSH-only login); see the previous item. **`37: Operation already in progress`** — a previous `bootout` is still tearing the job down; the installer and `hqssh service start` retry this automatically. **`133: Service is disabled`** — `launchctl enable gui/$(id -u)/com.gelotto.hqsshd`.

**Port 50051 is in use / "two daemons running".** The daemon retries a busy port for 5 s (an old instance draining its shutdown), then serves the Unix socket only and keeps retrying in the background; the log says so with an `ERROR` line and `hqssh doctor` fails the `tcp` check. Find the owner with `lsof -nP -iTCP:50051` (`/usr/sbin/lsof` on macOS). If two daemons answer, `hqssh doctor` reports which pid owns which transport. A daemon started by hand while the service is running exits with code 3 and prints the stop command.

**`auth_token` is set.** `hqssh doctor`, `hqssh service` and `hqssh version` read the token from `~/.hqssh/daemon.yaml` and send it, so they keep working; an `Unauthenticated` answer means the running daemon was started with a different token — restart it.

**Custom `socket:` or `tcp_port:`.** `hqssh doctor` and `hqssh service` read them from `daemon.yaml`; `-S` overrides the socket. The installer's own readiness probe uses the defaults, then defers to `hqssh doctor` for the verdict.

**Installing over SSH without a GUI session.** A user agent cannot be loaded into the (absent) GUI domain; the installer and `hqssh service start` load it into `user/<uid>` instead — it then runs while you have any session on the Mac and does not start at boot. `hqssh service status` says so.

**Stale `/tmp/hqssh.sock`.** After a SIGKILL or crash the socket file survives; the next start detects that nothing listens on it and removes it (`removing stale socket` in the log). `hqssh` reports "socket exists but nothing is listening" instead of a raw gRPC error. Remove it by hand only when no `hqsshd` process exists.

**Logs.** macOS: `~/.hqssh/logs/hqsshd.log` (launchd's stdout/stderr; rotated to `.1`–`.3` by the installer on upgrade when larger than 5 MB). Linux: `journalctl --user -u hqsshd`. `hqssh service logs -f` follows either. `hqsshd --log-level debug` (or `log.level` in the config) adds detail. When the daemon fails to start, the reason is the last `level=ERROR` line — `hqssh doctor` prints the tail when anything fails.

**Project discovery skips `~/Desktop`, `~/Documents`, `~/Downloads` (macOS).** Those folders are protected by TCC. Grant `hqsshd` Full Disk Access (System Settings › Privacy & Security › Full Disk Access; the installer signs the binary as `com.gelotto.hqsshd`, which is how it appears there), or keep projects elsewhere. On a headless Mac the login keychain stays locked until a GUI login, so tools that read credentials from it (Claude Code) may need one login after each reboot.

**Slow first start on macOS.** The binaries are ad-hoc signed (no Developer ID, no notarization), so Gatekeeper assesses them on first launch after each update — a few seconds, once. Kernel log lines like `AMFI: '.../hqsshd' has no CMS blob` are expected and harmless.

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

**Alternative: require an auth token.** Set `auth_token` in `~/.hqssh/daemon.yaml` and every RPC (on both listeners) must carry matching `authorization: Bearer <token>` metadata — other local users can no longer drive the daemon through the TCP port. Enter the same token in the HQSSH app's system settings (DAEMON AUTH TOKEN field, stored in the platform keystore). Validation uses a constant-time compare.

```yaml
# ~/.hqssh/daemon.yaml
auth_token: "generate-a-long-random-string-here"
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

# Regenerate proto (if modified; needs protoc — brew install protobuf / apt install protobuf-compiler)
make proto

# Build binaries
make build

# Run tests
make test

# Clean build artifacts
make clean
```

CI runs the Go tests on Linux and macOS, shellchecks the installer, and installs the built binaries for real — launchd on macOS, systemd on Linux — then restarts, upgrades and uninstalls them (`.github/workflows/ci.yml`, job `install-e2e`).

## Data Storage

All data is stored in `~/.hqssh/`:

| File | Purpose |
|------|---------|
| `daemon.yaml` | Configuration (optional) |
| `projects.json` | Registered projects |
| `discovery.json` | Learned scan roots and removed-project tombstones |
| `sessions.json` | Session records (for history and re-attach after restart) |
| `tasks.json` | Task definitions |
| `task_runs.json` | Task execution history |
| `hqsshd.pid` | Pid of the running daemon, locked with `flock` while it runs (removed on clean exit) |
| `logs/hqsshd.log` | Daemon log under launchd (macOS); Linux logs to journald |
| `logs/sessions/` | Per-session terminal logs |

## License

Apache License 2.0 - See [LICENSE](LICENSE)

## Contributing

Contributions welcome! Please open an issue to discuss significant changes before submitting a PR.
