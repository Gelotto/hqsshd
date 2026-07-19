#!/bin/sh
# hqsshd installer — https://hqssh.com
#
# Usage:
#   curl -fsSL https://hqssh.com/install | sh
#
# Environment variables:
#   HQSSH_VERSION      Pin a specific version (e.g. v0.3.0). Default: latest.
#   HQSSH_INSTALL_DIR  Override install directory. Default: ~/.local/bin
#   HQSSH_NO_SERVICE   Set to 1 to skip systemd/launchd service setup.
#   HQSSH_NO_START     Set to 1 to skip starting the service after install.
#
# Re-run this script to update. Pass --uninstall to remove.

set -eu

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
HQSSH_REPO="Gelotto/hqsshd"
INSTALL_DIR="${HQSSH_INSTALL_DIR:-$HOME/.local/bin}"
SERVICE_NAME="hqsshd"
SERVICE_DIR="$HOME/.config/systemd/user"
SERVICE_FILE="$SERVICE_DIR/$SERVICE_NAME.service"
LAUNCHD_LABEL="com.gelotto.hqsshd"
LAUNCHD_PLIST="$HOME/Library/LaunchAgents/${LAUNCHD_LABEL}.plist"
TMPDIR_BASE="${TMPDIR:-/tmp}"

# ---------------------------------------------------------------------------
# Colors (disabled when piped)
# ---------------------------------------------------------------------------
if [ -t 1 ]; then
    GREEN='\033[0;32m'
    YELLOW='\033[0;33m'
    RED='\033[0;31m'
    BOLD='\033[1m'
    RESET='\033[0m'
else
    GREEN='' YELLOW='' RED='' BOLD='' RESET=''
fi

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
info()  { printf "${GREEN}[hqssh]${RESET} %s\n" "$*"; }
warn()  { printf "${YELLOW}[hqssh]${RESET} %s\n" "$*" >&2; }
error() { printf "${RED}[hqssh]${RESET} %s\n" "$*" >&2; exit 1; }

WORK_DIR=""
cleanup() {
    [ -n "$WORK_DIR" ] && rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# Portable HTTP fetch — prefers curl, falls back to wget.
fetch() {
    _url="$1"
    _dest="${2:-}"
    if command -v curl >/dev/null 2>&1; then
        if [ -n "$_dest" ]; then
            curl -fsSL -o "$_dest" "$_url"
        else
            curl -fsSL "$_url"
        fi
    elif command -v wget >/dev/null 2>&1; then
        if [ -n "$_dest" ]; then
            wget -qO "$_dest" "$_url"
        else
            wget -qO- "$_url"
        fi
    else
        error "curl or wget is required"
    fi
}

# ---------------------------------------------------------------------------
# Steps
# ---------------------------------------------------------------------------

verify_prerequisites() {
    for cmd in tar; do
        command -v "$cmd" >/dev/null 2>&1 || error "'$cmd' is required but not found"
    done
    # Need curl or wget
    if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
        error "curl or wget is required"
    fi
    # Need a sha256 tool
    if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
        error "sha256sum or shasum is required for checksum verification"
    fi
}

detect_platform() {
    OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
    case "$OS" in
        linux)  OS="linux" ;;
        darwin) OS="darwin" ;;
        *)      error "Unsupported OS: $OS" ;;
    esac

    ARCH="$(uname -m)"
    case "$ARCH" in
        x86_64|amd64)   ARCH="amd64" ;;
        aarch64|arm64)  ARCH="arm64" ;;
        *)              error "Unsupported architecture: $ARCH" ;;
    esac

    info "Detected platform: ${OS}/${ARCH}"
}

resolve_version() {
    if [ -n "${HQSSH_VERSION:-}" ]; then
        VERSION="$HQSSH_VERSION"
        info "Using pinned version: $VERSION"
        return
    fi

    info "Resolving latest version..."
    _api_url="https://api.github.com/repos/${HQSSH_REPO}/releases/latest"
    _response="$(fetch "$_api_url" "" 2>/dev/null)" || {
        warn "GitHub API request failed. You may be rate-limited."
        warn "Set HQSSH_VERSION=vX.Y.Z or export GITHUB_TOKEN to authenticate."
        error "Could not determine latest version"
    }

    # Extract tag_name from JSON without jq
    VERSION="$(printf '%s' "$_response" | grep '"tag_name"' | head -1 | sed 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/')"
    [ -n "$VERSION" ] || error "Could not parse latest version from GitHub API"
    info "Latest version: $VERSION"
}

check_installed_version() {
    if [ -x "$INSTALL_DIR/hqsshd" ]; then
        CURRENT="$("$INSTALL_DIR/hqsshd" --version 2>/dev/null | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+[^ ]*' || echo "")"
        if [ -n "$CURRENT" ]; then
            if [ "$CURRENT" = "$VERSION" ]; then
                info "hqsshd $VERSION is already installed"
            else
                info "Upgrading hqsshd from $CURRENT to $VERSION"
            fi
            return
        fi
    fi
    info "Fresh install of hqsshd $VERSION"
}

download_archive() {
    WORK_DIR="$(mktemp -d "${TMPDIR_BASE}/hqssh-install.XXXXXX")"
    ARCHIVE_NAME="hqsshd-${OS}-${ARCH}.tar.gz"
    ARCHIVE_URL="https://github.com/${HQSSH_REPO}/releases/download/${VERSION}/${ARCHIVE_NAME}"
    CHECKSUMS_URL="https://github.com/${HQSSH_REPO}/releases/download/${VERSION}/checksums.txt"

    info "Downloading ${ARCHIVE_NAME}..."
    fetch "$ARCHIVE_URL" "$WORK_DIR/$ARCHIVE_NAME"

    info "Downloading checksums..."
    fetch "$CHECKSUMS_URL" "$WORK_DIR/checksums.txt"
}

verify_checksum() {
    info "Verifying checksum..."
    _expected="$(grep "$ARCHIVE_NAME" "$WORK_DIR/checksums.txt" | awk '{print $1}')"
    [ -n "$_expected" ] || error "No checksum found for $ARCHIVE_NAME in checksums.txt"

    if command -v sha256sum >/dev/null 2>&1; then
        _actual="$(sha256sum "$WORK_DIR/$ARCHIVE_NAME" | awk '{print $1}')"
    else
        _actual="$(shasum -a 256 "$WORK_DIR/$ARCHIVE_NAME" | awk '{print $1}')"
    fi

    if [ "$_expected" != "$_actual" ]; then
        error "Checksum mismatch!
  Expected: $_expected
  Actual:   $_actual
The archive may be corrupted or tampered with."
    fi
    info "Checksum verified"
}

stop_existing_service() {
    if [ "$OS" != "linux" ]; then return; fi
    if [ "${HQSSH_NO_SERVICE:-}" = "1" ]; then return; fi
    if ! command -v systemctl >/dev/null 2>&1; then return; fi

    # Ensure XDG_RUNTIME_DIR is set (may be missing in SSH sessions)
    if [ -z "${XDG_RUNTIME_DIR:-}" ]; then
        export XDG_RUNTIME_DIR="/run/user/$(id -u)"
    fi

    if systemctl --user is-active "$SERVICE_NAME" >/dev/null 2>&1; then
        info "Stopping existing hqsshd service..."
        systemctl --user stop "$SERVICE_NAME" 2>/dev/null || true
    fi
}

install_binaries() {
    mkdir -p "$INSTALL_DIR"

    info "Extracting binaries to $INSTALL_DIR..."
    tar -xzf "$WORK_DIR/$ARCHIVE_NAME" -C "$WORK_DIR"

    for bin in hqsshd hqssh; do
        [ -f "$WORK_DIR/$bin" ] || error "Binary '$bin' not found in archive"
        mv "$WORK_DIR/$bin" "$INSTALL_DIR/$bin"
        chmod +x "$INSTALL_DIR/$bin"
    done

    info "Installed: $INSTALL_DIR/hqsshd, $INSTALL_DIR/hqssh"
}

install_service() {
    if [ "${HQSSH_NO_SERVICE:-}" = "1" ]; then
        info "Skipping service setup (HQSSH_NO_SERVICE=1)"
        return
    fi
    case "$OS" in
        linux)  install_service_systemd ;;
        darwin) install_service_launchd ;;
    esac
}

install_service_systemd() {
    if ! command -v systemctl >/dev/null 2>&1; then
        warn "systemctl not found — skipping service setup"
        warn "Run hqsshd manually: $INSTALL_DIR/hqsshd"
        return
    fi

    mkdir -p "$SERVICE_DIR"

    cat > "$SERVICE_FILE" <<UNIT
[Unit]
Description=HQSSH Daemon - AI CLI Session Manager
Documentation=https://github.com/${HQSSH_REPO}
After=network.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/hqsshd
Restart=on-failure
RestartSec=5
Environment=HOME=%h

# Logging goes to journald
# View with: journalctl --user -u hqsshd -f
StandardOutput=journal
StandardError=journal

# Security hardening
# NOTE: ProtectHome is intentionally omitted — AI tools (claude, codex, aider)
# need write access to project directories under \$HOME.
# NOTE: PrivateTmp and ProtectSystem are intentionally omitted. For user
# units, any mount-namespace option puts the service in an unprivileged user
# namespace, which (a) hides /tmp/hqssh.sock from local CLI tools and
# (b) blocks reading /proc/<pid>/cwd of the user's other processes, breaking
# external session discovery (ListExternalSessions).
NoNewPrivileges=true

[Install]
WantedBy=default.target
UNIT

    info "Installed systemd service: $SERVICE_FILE"
}

install_service_launchd() {
    if ! command -v launchctl >/dev/null 2>&1; then
        warn "launchctl not found — skipping service setup"
        warn "Run hqsshd manually: $INSTALL_DIR/hqsshd"
        return
    fi

    mkdir -p "$HOME/Library/LaunchAgents"
    mkdir -p "$HOME/.hqssh/logs"

    cat > "$LAUNCHD_PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>${LAUNCHD_LABEL}</string>
    <key>ProgramArguments</key>
    <array>
        <string>${INSTALL_DIR}/hqsshd</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>StandardOutPath</key>
    <string>${HOME}/.hqssh/logs/hqsshd.log</string>
    <key>StandardErrorPath</key>
    <string>${HOME}/.hqssh/logs/hqsshd.log</string>
    <key>WorkingDirectory</key>
    <string>${HOME}</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>${HOME}</string>
    </dict>
</dict>
</plist>
PLIST

    chmod 644 "$LAUNCHD_PLIST"
    info "Installed launchd agent: $LAUNCHD_PLIST"
}

start_service() {
    if [ "${HQSSH_NO_SERVICE:-}" = "1" ]; then return; fi
    if [ "${HQSSH_NO_START:-}" = "1" ]; then
        info "Skipping service start (HQSSH_NO_START=1)"
        return
    fi
    case "$OS" in
        linux)  start_service_systemd ;;
        darwin) start_service_launchd ;;
    esac
}

start_service_systemd() {
    if ! command -v systemctl >/dev/null 2>&1; then return; fi

    # Ensure XDG_RUNTIME_DIR is set (may be missing in SSH sessions)
    if [ -z "${XDG_RUNTIME_DIR:-}" ]; then
        export XDG_RUNTIME_DIR="/run/user/$(id -u)"
    fi

    systemctl --user daemon-reload

    # Enable linger so user services survive logout (critical for SSH daemon)
    if command -v loginctl >/dev/null 2>&1; then
        loginctl enable-linger "$(whoami)" 2>/dev/null || warn "Could not enable linger — service may stop on logout"
    fi

    systemctl --user enable "$SERVICE_NAME" 2>/dev/null
    systemctl --user restart "$SERVICE_NAME"
    info "Service started"
}

start_service_launchd() {
    if ! command -v launchctl >/dev/null 2>&1; then return; fi
    [ -f "$LAUNCHD_PLIST" ] || return

    _uid="$(id -u)"

    # Unload any prior instance so the new plist/binary are picked up.
    launchctl bootout "gui/${_uid}/${LAUNCHD_LABEL}" 2>/dev/null || true
    launchctl unload "$LAUNCHD_PLIST" 2>/dev/null || true

    # Try modern bootstrap first (macOS 10.11+); fall back to legacy load -w,
    # which works in SSH sessions where the gui/$UID domain may not exist.
    if launchctl bootstrap "gui/${_uid}" "$LAUNCHD_PLIST" 2>/dev/null; then
        :
    elif launchctl load -w "$LAUNCHD_PLIST" 2>/dev/null; then
        :
    else
        warn "Could not load launchd agent automatically."
        warn "Run: launchctl load \"$LAUNCHD_PLIST\""
        return
    fi

    # Force-start now instead of waiting for next login event.
    launchctl kickstart -k "gui/${_uid}/${LAUNCHD_LABEL}" 2>/dev/null || true
    info "Service started"
}

check_path() {
    case ":${PATH}:" in
        *":${INSTALL_DIR}:"*) ;;
        *)
            warn "$INSTALL_DIR is not in your PATH"
            warn "Add it with:  export PATH=\"$INSTALL_DIR:\$PATH\""
            warn "Or add to your shell profile (~/.bashrc, ~/.zshrc, etc.)"
            ;;
    esac
}

print_summary() {
    _version="$("$INSTALL_DIR/hqsshd" --version 2>/dev/null || echo "$VERSION")"
    printf "\n"
    info "${BOLD}Installation complete!${RESET}"
    printf "\n"
    printf "  Version:   %s\n" "$_version"
    printf "  Binaries:  %s/hqsshd, %s/hqssh\n" "$INSTALL_DIR" "$INSTALL_DIR"
    if [ "$OS" = "linux" ] && [ "${HQSSH_NO_SERVICE:-}" != "1" ] && command -v systemctl >/dev/null 2>&1; then
        printf "  Service:   %s\n" "$SERVICE_FILE"
        printf "\n"
        printf "  Useful commands:\n"
        printf "    journalctl --user -u hqsshd -f    # View logs\n"
        printf "    systemctl --user status hqsshd     # Check status\n"
        printf "    systemctl --user restart hqsshd    # Restart\n"
    elif [ "$OS" = "darwin" ] && [ "${HQSSH_NO_SERVICE:-}" != "1" ] && command -v launchctl >/dev/null 2>&1; then
        printf "  Service:   %s (launchd user agent)\n" "$LAUNCHD_LABEL"
        printf "\n"
        printf "  Useful commands:\n"
        printf "    tail -f ~/.hqssh/logs/hqsshd.log    # View logs\n"
        printf "    launchctl list | grep hqsshd        # Check status\n"
        printf "    launchctl kickstart -k gui/\$(id -u)/%s   # Restart\n" "$LAUNCHD_LABEL"
        printf "\n"
        printf "  Note: LaunchAgents auto-start on GUI login. After an unattended reboot\n"
        printf "  with no one logged into the Mac, the daemon won't start until someone\n"
        printf "  logs in at the console.\n"
    elif [ "$OS" = "darwin" ]; then
        printf "\n"
        printf "  To start the daemon manually:\n"
        printf "    %s/hqsshd\n" "$INSTALL_DIR"
    fi
    printf "\n"
}

# ---------------------------------------------------------------------------
# Uninstall
# ---------------------------------------------------------------------------

uninstall() {
    info "Uninstalling hqsshd..."

    _os="$(uname -s | tr '[:upper:]' '[:lower:]')"

    # Linux: stop, disable, and remove the systemd user service.
    if [ "$_os" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
        if [ -z "${XDG_RUNTIME_DIR:-}" ]; then
            export XDG_RUNTIME_DIR="/run/user/$(id -u)"
        fi
        systemctl --user stop "$SERVICE_NAME" 2>/dev/null || true
        systemctl --user disable "$SERVICE_NAME" 2>/dev/null || true
        rm -f "$SERVICE_FILE"
        systemctl --user daemon-reload 2>/dev/null || true
    fi

    # macOS: unload and remove the launchd agent.
    if [ "$_os" = "darwin" ] && command -v launchctl >/dev/null 2>&1; then
        launchctl bootout "gui/$(id -u)/${LAUNCHD_LABEL}" 2>/dev/null || true
        launchctl unload "$LAUNCHD_PLIST" 2>/dev/null || true
        rm -f "$LAUNCHD_PLIST"
    fi

    # Remove binaries
    rm -f "$INSTALL_DIR/hqsshd" "$INSTALL_DIR/hqssh"

    info "Uninstalled. Data in ~/.hqssh/ was preserved."
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
    # Handle --uninstall flag
    for arg in "$@"; do
        case "$arg" in
            --uninstall)
                uninstall
                exit 0
                ;;
            --help|-h)
                printf "Usage: install.sh [--uninstall | --help]\n\n"
                printf "Install or update hqsshd.\n\n"
                printf "Environment variables:\n"
                printf "  HQSSH_VERSION       Pin a specific version (default: latest)\n"
                printf "  HQSSH_INSTALL_DIR   Override install directory (default: ~/.local/bin)\n"
                printf "  HQSSH_NO_SERVICE    Set to 1 to skip systemd/launchd service setup\n"
                printf "  HQSSH_NO_START      Set to 1 to skip starting the service\n"
                exit 0
                ;;
        esac
    done

    info "hqsshd installer"
    verify_prerequisites
    detect_platform
    resolve_version
    check_installed_version
    download_archive
    verify_checksum
    stop_existing_service
    install_binaries
    install_service
    start_service
    check_path
    print_summary
}

main "$@"
