#!/bin/sh
# shellcheck shell=sh
# hqsshd installer — https://hqssh.com
#
# Usage:
#   curl -fsSL https://hqssh.com/install | sh
#   curl -fsSL https://hqssh.com/install | HQSSH_SERVICE_SCOPE=system sh   # macOS: start at boot
#   sh install.sh [--system] [--uninstall [--purge-logs]] [--help]
#
# Environment variables:
#   HQSSH_VERSION        Pin a specific version (e.g. v1.4.0). Default: latest.
#   HQSSH_INSTALL_DIR    Override install directory. Default: ~/.local/bin
#   HQSSH_SERVICE_SCOPE  user (default) | system. macOS only: "system" installs a
#                        LaunchDaemon that starts at boot without a GUI login (uses sudo).
#   HQSSH_NO_SERVICE     Set to 1 to skip systemd/launchd service setup.
#   HQSSH_NO_START       Set to 1 to skip starting the service after install.
#   HQSSH_LOCAL_ARCHIVE  Path to a hqsshd-<os>-<arch>.tar.gz to install instead of
#                        downloading (skips checksum verification; for CI/development).
#
# Exit codes:
#   0  success
#   1  error; nothing was installed (or download/verification failed)
#   2  usage error
#   3  binaries installed, but the service did not start or did not pass its health check
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
SYSTEM_PLIST="/Library/LaunchDaemons/${LAUNCHD_LABEL}.plist"
SERVICE_SCOPE="${HQSSH_SERVICE_SCOPE:-user}"
DATA_DIR="$HOME/.hqssh"
LOG_DIR="$DATA_DIR/logs"
LOG_FILE="$LOG_DIR/hqsshd.log"
PIDFILE="$DATA_DIR/hqsshd.pid"
SOCKET_PATH="/tmp/hqssh.sock"
TCP_PORT=50051
TMPDIR_BASE="${TMPDIR:-/tmp}"
PURGE_LOGS=0
LOCAL_ARCHIVE=0
HAVE_DOCTOR=""
LAUNCHCTL_ERR=""

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
info()  { printf '%b[hqssh]%b %s\n' "$GREEN" "$RESET" "$*"; }
warn()  { printf '%b[hqssh]%b %s\n' "$YELLOW" "$RESET" "$*" >&2; }
error() { printf '%b[hqssh]%b %s\n' "$RED" "$RESET" "$*" >&2; exit 1; }

WORK_DIR=""
cleanup() {
    if [ -n "$WORK_DIR" ]; then
        rm -rf "$WORK_DIR"
    fi
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

# run_sudo prints the command before sudo runs it, so a password prompt is
# never a surprise. Only HQSSH_SERVICE_SCOPE=system ever needs it.
run_sudo() {
    info "  sudo $*"
    sudo "$@"
}

# ensure_sudo validates sudo once up front. sudo prompts on /dev/tty, which
# works under `curl | sh` from a terminal; without one it cannot ask.
ensure_sudo() {
    if sudo -n true 2>/dev/null; then
        return 0
    fi
    if sudo -v; then
        return 0
    fi
    error "HQSSH_SERVICE_SCOPE=system needs sudo. Run 'sudo -v' first (or run from a terminal), then re-run the installer."
}

# binary_version prints the version of an hqsshd/hqssh binary, normalised
# to a leading "v": release tags ("v1.4.0"), local builds ("1.4.0-3-gabc-dirty"
# → "v1.4.0-3-gabc-dirty"), untagged builds ("abc1234" → "dev-abc1234").
binary_version() {
    _v="$("$1" --version 2>/dev/null | awk '{print $NF}')"
    case "$_v" in
        "") printf '' ;;
        v[0-9]*) printf '%s' "$_v" ;;
        [0-9]*.[0-9]*.[0-9]*) printf 'v%s' "$_v" ;;
        *) printf 'dev-%s' "$_v" ;;
    esac
}

# ---------------------------------------------------------------------------
# Steps
# ---------------------------------------------------------------------------

verify_prerequisites() {
    command -v tar >/dev/null 2>&1 || error "'tar' is required but not found"
    if [ "$LOCAL_ARCHIVE" = 0 ]; then
        if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
            error "curl or wget is required"
        fi
        if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
            error "sha256sum or shasum is required for checksum verification"
        fi
    fi
    if [ "$(id -u)" = 0 ]; then
        if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ]; then
            error "Run the installer as $SUDO_USER, not under sudo: the daemon must belong to the user whose sessions it hosts. It calls sudo itself when HQSSH_SERVICE_SCOPE=system."
        fi
        warn "Installing for root: the daemon and its sessions will run as root."
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

    case "$SERVICE_SCOPE" in
        user) ;;
        system)
            if [ "$OS" != "darwin" ]; then
                error "HQSSH_SERVICE_SCOPE=system is macOS-only. On Linux the user service already starts at boot via 'loginctl enable-linger' (the installer runs it)."
            fi
            ;;
        *) error "HQSSH_SERVICE_SCOPE must be 'user' or 'system' (got '$SERVICE_SCOPE')" ;;
    esac

    info "Detected platform: ${OS}/${ARCH}"
}

resolve_version() {
    if [ "$LOCAL_ARCHIVE" = 1 ]; then
        [ -f "$HQSSH_LOCAL_ARCHIVE" ] || error "HQSSH_LOCAL_ARCHIVE not found: $HQSSH_LOCAL_ARCHIVE"
        VERSION="local"
        info "Using local archive: $HQSSH_LOCAL_ARCHIVE"
        return
    fi

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
        CURRENT="$(binary_version "$INSTALL_DIR/hqsshd")"
        if [ -n "$CURRENT" ]; then
            if [ "$VERSION" = local ]; then
                info "Replacing hqsshd $CURRENT with the local archive"
            elif [ "$CURRENT" = "$VERSION" ]; then
                info "hqsshd $VERSION is already installed (reinstalling; the service definition is refreshed)"
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

    if [ "$LOCAL_ARCHIVE" = 1 ]; then
        if [ "$(basename "$HQSSH_LOCAL_ARCHIVE")" != "$ARCHIVE_NAME" ]; then
            warn "Local archive is named $(basename "$HQSSH_LOCAL_ARCHIVE"), expected $ARCHIVE_NAME for this platform"
        fi
        cp "$HQSSH_LOCAL_ARCHIVE" "$WORK_DIR/$ARCHIVE_NAME"
        return
    fi

    ARCHIVE_URL="https://github.com/${HQSSH_REPO}/releases/download/${VERSION}/${ARCHIVE_NAME}"
    CHECKSUMS_URL="https://github.com/${HQSSH_REPO}/releases/download/${VERSION}/checksums.txt"

    info "Downloading ${ARCHIVE_NAME}..."
    fetch "$ARCHIVE_URL" "$WORK_DIR/$ARCHIVE_NAME"

    info "Downloading checksums..."
    fetch "$CHECKSUMS_URL" "$WORK_DIR/checksums.txt"
}

verify_checksum() {
    if [ "$LOCAL_ARCHIVE" = 1 ]; then
        info "Skipping checksum verification (local archive)"
        return
    fi

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

# ---------------------------------------------------------------------------
# Process / service probes
# ---------------------------------------------------------------------------

# running_daemon_pids lists hqsshd processes owned by this user (never other
# users'). -x matches the exact name, so hqssh (the CLI) is not included.
running_daemon_pids() {
    pgrep -x -u "$(id -u)" hqsshd 2>/dev/null || true
}

# alive_pids prints those of the given pids that still exist.
alive_pids() {
    for _p in "$@"; do
        if kill -0 "$_p" 2>/dev/null; then
            printf '%s\n' "$_p"
        fi
    done
}

# wait_for_pids waits up to $1 seconds for the remaining pids to exit.
# Returns 1 if one is still alive.
wait_for_pids() {
    _secs="$1"
    shift
    _i=0
    while [ "$_i" -lt "$_secs" ]; do
        [ -z "$(alive_pids "$@")" ] && return 0
        if [ $((_i % 5)) -eq 0 ] && [ "$_i" -gt 0 ]; then
            info "  waiting for hqsshd to exit (${_i}s)..."
        fi
        sleep 1
        _i=$((_i + 1))
    done
    [ -z "$(alive_pids "$@")" ]
}

# pidfile_pid prints the pid recorded by a v1.4.0+ daemon, if that process
# still exists.
pidfile_pid() {
    [ -f "$PIDFILE" ] || return 0
    _p="$(tr -d '[:space:]' < "$PIDFILE" 2>/dev/null)"
    case "$_p" in
        ''|*[!0-9]*) return 0 ;;
    esac
    kill -0 "$_p" 2>/dev/null && printf '%s' "$_p"
    return 0
}

# launchd_find_domain prints the launchd domain the job is loaded in:
# gui/<uid> (normal), user/<uid> (where the old installer's `launchctl load
# -w` fallback put it), system (LaunchDaemon), or nothing.
launchd_find_domain() {
    _uid="$(id -u)"
    for _d in "gui/$_uid" "user/$_uid" system; do
        if launchctl print "$_d/$LAUNCHD_LABEL" >/dev/null 2>&1; then
            printf '%s' "$_d"
            return 0
        fi
    done
    return 0
}

# launchd_service_field prints one top-level "key = value" of the job in
# domain $1 (e.g. pid, state, "last exit code").
launchd_service_field() {
    launchctl print "$1/$LAUNCHD_LABEL" 2>/dev/null | sed -n "s/^[[:space:]]*$2 = \(.*\)/\1/p" | head -1
}

# launchd_service_pid prints the pid launchd reports for the job in domain $1.
launchd_service_pid() {
    launchd_service_field "$1" pid
}

# STOP_WAIT_SECS is how long to wait for a daemon to exit after asking:
# the plist's ExitTimeOut (30) plus slack; old plists have launchd's 5.
STOP_WAIT_SECS=35

# stop_daemon_processes waits for the given pids (the daemon being
# replaced — never other hqsshd instances with their own data dirs) to
# exit, escalating to SIGKILL after STOP_WAIT_SECS. Returns 1 if one
# survives.
stop_daemon_processes() {
    [ $# -gt 0 ] || return 0
    if ! wait_for_pids "$STOP_WAIT_SECS" "$@"; then
        warn "hqsshd did not exit in time; sending SIGKILL"
        # shellcheck disable=SC2046 # intentional word split: one pid per argument
        kill -KILL $(alive_pids "$@") 2>/dev/null || true
        sleep 1
    fi
    [ -z "$(alive_pids "$@")" ]
}

# daemon_pids_to_stop prints the pids of the daemon this installer manages:
# the service's process, else the pidfile's (a daemon run by hand), else —
# for pre-1.4.0 daemons that wrote no pidfile — every hqsshd of this user.
# $1 = the service pid if known.
daemon_pids_to_stop() {
    if [ -n "${1:-}" ]; then
        printf '%s' "$1"
        return 0
    fi
    _p="$(pidfile_pid)"
    if [ -n "$_p" ]; then
        printf '%s' "$_p"
        return 0
    fi
    running_daemon_pids
}

# gui_domain_exists: the domain a LaunchAgent is bootstrapped into. Absent
# until someone logs in at the Mac's console.
gui_domain_exists() {
    launchctl print "gui/$(id -u)" >/dev/null 2>&1
}

console_user() {
    stat -f %Su /dev/console 2>/dev/null || printf 'unknown'
}

print_no_gui_caveat() {
    printf '\n'
    warn "LaunchAgents start at GUI login, not at boot. Console user now: $(console_user); you: $(id -un)${SSH_CONNECTION:+ (over SSH)}."
    warn "After a reboot the daemon only starts once someone logs in at the Mac's console."
    warn "For a daemon that starts at boot and survives logout, install it as a system daemon:"
    warn "    curl -fsSL https://hqssh.com/install | HQSSH_SERVICE_SCOPE=system sh"
    warn "  (or: sh install.sh --system). Linux analog: loginctl enable-linger."
}

# have_doctor caches whether the installed hqssh knows `doctor` (v1.4.0+).
have_doctor() {
    if [ -z "$HAVE_DOCTOR" ]; then
        if "$INSTALL_DIR/hqssh" doctor --help >/dev/null 2>&1; then
            HAVE_DOCTOR=0
        else
            HAVE_DOCTOR=1
        fi
    fi
    return "$HAVE_DOCTOR"
}

port_open() {
    if command -v nc >/dev/null 2>&1; then
        nc -z -w 1 127.0.0.1 "$TCP_PORT" >/dev/null 2>&1
    elif command -v ss >/dev/null 2>&1; then
        ss -Hltn "sport = :$TCP_PORT" 2>/dev/null | grep -q .
    else
        return 0
    fi
}

# service_pid_any prints the daemon pid as the service manager (or the
# pidfile) sees it.
service_pid_any() {
    case "$OS" in
        darwin)
            _d="$(launchd_find_domain)"
            [ -n "$_d" ] && launchd_service_pid "$_d"
            ;;
        linux)
            if command -v systemctl >/dev/null 2>&1; then
                _p="$(systemctl --user show -p MainPID --value "$SERVICE_NAME" 2>/dev/null || true)"
                [ "${_p:-0}" != 0 ] && printf '%s' "$_p"
            fi
            ;;
    esac
    if [ -f "$PIDFILE" ]; then
        cat "$PIDFILE" 2>/dev/null
    fi
    return 0
}

# daemon_ready is the cheap readiness signal polled once a second: the
# socket exists and the TCP port answers (a daemon whose port is held by
# something else serves socket-only and keeps retrying; doctor reports it).
daemon_ready() {
    [ -S "$SOCKET_PATH" ] && port_open
}

# wait_for_healthy polls readiness for up to $1 seconds, then runs
# `hqssh doctor --quiet` once (v1.4.0+) for the verdict.
wait_for_healthy() {
    _i=0
    while [ "$_i" -lt "$1" ]; do
        daemon_ready && break
        sleep 1
        _i=$((_i + 1))
    done
    if have_doctor; then
        "$INSTALL_DIR/hqssh" doctor --quiet >/dev/null 2>&1
        return
    fi
    daemon_ready && [ -n "$(service_pid_any)" ]
}

# running_version reads the version the daemon logged when it started.
running_version() {
    if [ "$OS" = darwin ]; then
        _line="$(grep 'hqsshd starting' "$LOG_FILE" 2>/dev/null | tail -1)"
    else
        _line="$(journalctl --user -u "$SERVICE_NAME" -n 50 --no-pager 2>/dev/null | grep 'hqsshd starting' | tail -1)"
    fi
    _v="$(printf '%s' "$_line" | sed -n 's/.*version=\([^ ]*\).*/\1/p')"
    if [ -n "$_v" ]; then
        printf '%s' "$_v"
    else
        binary_version "$INSTALL_DIR/hqsshd"
    fi
}

# rotate_log keeps launchd's log file bounded. launchd holds it open with
# O_APPEND and cannot be told to reopen, so the only safe moment is while
# the daemon is stopped — i.e. here, during an upgrade.
rotate_log() {
    [ -f "$LOG_FILE" ] || return 0
    _size="$(wc -c < "$LOG_FILE" | tr -d ' ')"
    [ "${_size:-0}" -gt 5242880 ] || return 0
    info "Rotating $LOG_FILE ($((_size / 1024)) KB)"
    [ -f "$LOG_FILE.2" ] && mv -f "$LOG_FILE.2" "$LOG_FILE.3"
    [ -f "$LOG_FILE.1" ] && mv -f "$LOG_FILE.1" "$LOG_FILE.2"
    mv -f "$LOG_FILE" "$LOG_FILE.1"
    return 0
}

# ---------------------------------------------------------------------------
# Stop the running daemon before replacing it
# ---------------------------------------------------------------------------

stop_existing_service() {
    if [ "${HQSSH_NO_SERVICE:-}" = "1" ]; then return; fi
    case "$OS" in
        darwin) stop_existing_launchd ;;
        linux)  stop_existing_systemd ;;
    esac
}

stop_existing_launchd() {
    command -v launchctl >/dev/null 2>&1 || return 0

    _domain="$(launchd_find_domain)"
    _svc_pid=""
    if [ -n "$_domain" ]; then
        _svc_pid="$(launchd_service_pid "$_domain")"
        info "Stopping hqsshd ($_domain/$LAUNCHD_LABEL${_svc_pid:+, pid $_svc_pid})..."
        if [ "$_domain" = system ]; then
            ensure_sudo
            run_sudo launchctl bootout "system/$LAUNCHD_LABEL" || true
        else
            launchctl bootout "$_domain/$LAUNCHD_LABEL" 2>/dev/null || true
        fi
    fi

    # shellcheck disable=SC2046 # intentional word split: one pid per argument
    set -- $(daemon_pids_to_stop "$_svc_pid")
    if [ -z "$_domain" ] && [ $# -gt 0 ]; then
        warn "hqsshd is running outside launchd (pid $*) — stopping it so the new binary can start"
        kill -TERM "$@" 2>/dev/null || true
    fi

    if ! stop_daemon_processes "$@"; then
        error "hqsshd is still running (pid $(alive_pids "$@" | tr '\n' ' ')); refusing to overwrite a live binary"
    fi
    rotate_log
}

stop_existing_systemd() {
    command -v systemctl >/dev/null 2>&1 || return 0
    systemd_env

    _svc_pid=""
    if systemctl --user is-active "$SERVICE_NAME" >/dev/null 2>&1; then
        _svc_pid="$(systemctl --user show -p MainPID --value "$SERVICE_NAME" 2>/dev/null || true)"
        [ "${_svc_pid:-0}" = 0 ] && _svc_pid=""
        info "Stopping existing hqsshd service${_svc_pid:+ (pid $_svc_pid)}..."
        systemctl --user stop "$SERVICE_NAME" 2>/dev/null || true
    fi
    # shellcheck disable=SC2046 # intentional word split: one pid per argument
    set -- $(daemon_pids_to_stop "$_svc_pid")
    if [ -z "$_svc_pid" ] && [ $# -gt 0 ]; then
        warn "hqsshd is running outside systemd (pid $*) — stopping it so the new binary can start"
        kill -TERM "$@" 2>/dev/null || true
    fi
    if ! stop_daemon_processes "$@"; then
        error "hqsshd is still running (pid $(alive_pids "$@" | tr '\n' ' ')); refusing to overwrite a live binary"
    fi
}

# systemd_env fills in the variables `systemctl --user` needs when the
# installer runs from an SSH session without a full login environment.
systemd_env() {
    if [ -z "${XDG_RUNTIME_DIR:-}" ]; then
        XDG_RUNTIME_DIR="/run/user/$(id -u)"
        export XDG_RUNTIME_DIR
    fi
    if [ -z "${DBUS_SESSION_BUS_ADDRESS:-}" ]; then
        DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG_RUNTIME_DIR/bus"
        export DBUS_SESSION_BUS_ADDRESS
    fi
}

# ---------------------------------------------------------------------------
# Binaries
# ---------------------------------------------------------------------------

install_binaries() {
    mkdir -p "$INSTALL_DIR"

    info "Extracting binaries to $INSTALL_DIR..."
    tar -xzf "$WORK_DIR/$ARCHIVE_NAME" -C "$WORK_DIR"

    for bin in hqsshd hqssh; do
        [ -f "$WORK_DIR/$bin" ] || error "Binary '$bin' not found in archive"
        chmod +x "$WORK_DIR/$bin"
    done

    if [ "$OS" = darwin ]; then
        # Release binaries carry the Go linker's ad-hoc signature with the
        # identifier "a.out". Re-sign (still ad-hoc: no Gatekeeper skip) with
        # stable identifiers so launchd, TCC prompts, Console and `log show`
        # attribute the process to com.gelotto.hqsshd instead of "a.out".
        if command -v codesign >/dev/null 2>&1; then
            codesign --force --sign - --identifier "$LAUNCHD_LABEL" "$WORK_DIR/hqsshd" 2>/dev/null \
                || warn "codesign failed for hqsshd (continuing with the linker signature)"
            codesign --force --sign - --identifier "com.gelotto.hqssh" "$WORK_DIR/hqssh" 2>/dev/null || true
        fi
        # curl does not set the quarantine flag; a browser download would.
        xattr -d com.apple.quarantine "$WORK_DIR/hqsshd" "$WORK_DIR/hqssh" 2>/dev/null || true
    fi

    # mv is a rename on the same volume: atomic, and a stopped daemon's old
    # inode is untouched.
    for bin in hqsshd hqssh; do
        mv "$WORK_DIR/$bin" "$INSTALL_DIR/$bin"
    done

    if [ "$VERSION" = local ]; then
        VERSION="$(binary_version "$INSTALL_DIR/hqsshd")"
    fi
    HAVE_DOCTOR=""
    info "Installed: $INSTALL_DIR/hqsshd, $INSTALL_DIR/hqssh"
}

# ---------------------------------------------------------------------------
# Service definitions
# ---------------------------------------------------------------------------

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

    # Keep this identical to hqsshd.service in the repo (CI diffs them,
    # ignoring ExecStart).
    cat > "$SERVICE_FILE" <<UNIT
[Unit]
Description=HQSSH Daemon - AI CLI Session Manager
Documentation=https://github.com/Gelotto/hqsshd
After=network.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/hqsshd
Restart=on-failure
RestartSec=5
# The daemon's graceful stop is bounded at ~10s; give it room before SIGKILL.
TimeoutStopSec=30
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

# write_launchd_plist writes the job definition to stdout. $1 = "user" |
# "system". The system variant adds UserName/GroupName so the daemon runs
# as the installing user even though launchd starts it as root at boot.
write_launchd_plist() {
    cat <<PLIST
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
PLIST
    if [ "$1" = system ]; then
        cat <<PLIST
    <!-- Run as the installing user, not root. HOME is set explicitly below:
         LaunchDaemons get no per-user environment. -->
    <key>UserName</key>
    <string>$(id -un)</string>
    <key>GroupName</key>
    <string>$(id -gn)</string>
PLIST
    fi
    cat <<PLIST
    <key>RunAtLoad</key>
    <true/>
    <!-- Restart after a crash or non-zero exit. A clean exit 0 (what
         'hqssh service stop' triggers) stays stopped. -->
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
        <key>Crashed</key>
        <true/>
    </dict>
    <!-- Seconds between SIGTERM and SIGKILL. The macOS default (5) killed the
         daemon mid-shutdown; its graceful stop is bounded at ~10s. -->
    <key>ExitTimeOut</key>
    <integer>30</integer>
    <!-- Minimum seconds between respawns (default 10). -->
    <key>ThrottleInterval</key>
    <integer>5</integer>
    <key>WorkingDirectory</key>
    <string>${HOME}</string>
    <!-- launchd opens these O_APPEND; stdout and stderr share one file so a
         panic lands next to the structured log. Rotated by the installer on
         upgrade (hqsshd.log.1..3). launchd does not create the directory. -->
    <key>StandardOutPath</key>
    <string>${LOG_FILE}</string>
    <key>StandardErrorPath</key>
    <string>${LOG_FILE}</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>${HOME}</string>
        <!-- launchd's default PATH is /usr/bin:/bin:/usr/sbin:/sbin. The daemon
             also appends user tool directories at startup; a complete PATH
             here covers older binaries and lsof/git lookups. -->
        <key>PATH</key>
        <string>${INSTALL_DIR}:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
    <!-- ProcessType intentionally unset (= Standard). "Background" applies
         CPU/IO throttling inherited by every PTY child (claude, codex) and
         makes interactive sessions sluggish. -->
</dict>
</plist>
PLIST
}

install_service_launchd() {
    if ! command -v launchctl >/dev/null 2>&1; then
        warn "launchctl not found — skipping service setup"
        warn "Run hqsshd manually: $INSTALL_DIR/hqsshd"
        return
    fi

    # launchd needs the log file's directory to exist, and for system scope
    # the file must be owned by the user (launchd recreates a missing one as
    # root, which then blocks the next upgrade).
    mkdir -p "$LOG_DIR"
    if ! touch "$LOG_FILE" 2>/dev/null; then
        if [ "$SERVICE_SCOPE" = system ] || [ -f "$SYSTEM_PLIST" ]; then
            warn "$LOG_FILE is not writable (root-owned after a launchd restart); fixing ownership"
            ensure_sudo
            run_sudo chown "$(id -un)" "$LOG_FILE"
            touch "$LOG_FILE"
        else
            error "$LOG_FILE is not writable; fix its ownership and re-run"
        fi
    fi

    if [ "$SERVICE_SCOPE" = system ]; then
        ensure_sudo
        write_launchd_plist system > "$WORK_DIR/plist"
        run_sudo cp "$WORK_DIR/plist" "$SYSTEM_PLIST"
        run_sudo chown root:wheel "$SYSTEM_PLIST"
        run_sudo chmod 644 "$SYSTEM_PLIST"
        if [ -f "$LAUNCHD_PLIST" ]; then
            info "Removing the user agent plist ($LAUNCHD_PLIST); the system daemon replaces it"
            rm -f "$LAUNCHD_PLIST"
        fi
        info "Installed launchd system daemon: $SYSTEM_PLIST"
    else
        mkdir -p "$HOME/Library/LaunchAgents"
        write_launchd_plist user > "$LAUNCHD_PLIST"
        chmod 644 "$LAUNCHD_PLIST"
        if [ -f "$SYSTEM_PLIST" ]; then
            warn "A system daemon plist exists at $SYSTEM_PLIST; removing it so only the user agent is loaded"
            ensure_sudo
            run_sudo rm -f "$SYSTEM_PLIST"
        fi
        info "Installed launchd user agent: $LAUNCHD_PLIST"
    fi
}

# ---------------------------------------------------------------------------
# Start + verify
# ---------------------------------------------------------------------------

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
    command -v systemctl >/dev/null 2>&1 || return 0
    systemd_env

    if ! _err="$(systemctl --user daemon-reload 2>&1)"; then
        warn "systemctl --user daemon-reload failed: $_err"
        case "$_err" in
            *"connect to bus"*|*"No medium"*|*"not been booted"*)
                warn "No systemd user session for $(id -un). Enable one and log in once, or run:"
                warn "    loginctl enable-linger $(id -un) && sudo systemctl start user@$(id -u)"
                ;;
        esac
        service_failed_systemd
    fi

    # Enable linger so user services survive logout and start at boot
    if command -v loginctl >/dev/null 2>&1; then
        loginctl enable-linger "$(id -un)" 2>/dev/null || warn "Could not enable linger — the service may stop on logout"
    fi

    systemctl --user enable "$SERVICE_NAME" 2>/dev/null || true
    if ! _err="$(systemctl --user start "$SERVICE_NAME" 2>&1)"; then
        warn "systemctl --user start $SERVICE_NAME failed: $_err"
        service_failed_systemd
    fi

    if wait_for_healthy 20; then
        info "Service started (pid $(systemctl --user show -p MainPID --value "$SERVICE_NAME"), version $(running_version))"
    else
        service_failed_systemd
    fi
}

service_failed_systemd() {
    printf '\n' >&2
    if have_doctor; then
        warn "The service did not pass its health check:"
        "$INSTALL_DIR/hqssh" doctor >&2 || true
        printf '\n' >&2
    fi
    warn "systemd says:"
    systemctl --user status "$SERVICE_NAME" --no-pager 2>&1 | head -12 >&2 || true
    warn "Last log lines:"
    journalctl --user -u "$SERVICE_NAME" -n 30 --no-pager 2>&1 >&2 || true
    printf '\n' >&2
    warn "Next steps:"
    warn "    $INSTALL_DIR/hqssh doctor"
    warn "    systemctl --user status $SERVICE_NAME"
    warn "    journalctl --user -u $SERVICE_NAME -f"
    exit 3
}

# pick_agent_domain prints the domain a user agent can be loaded into from
# here: gui/<uid> when a GUI login session exists, else user/<uid> (where
# the legacy `launchctl load -w` fallback put it) — the daemon then runs
# for as long as the user has any session, and does not start at boot.
pick_agent_domain() {
    if gui_domain_exists; then
        printf 'gui/%s' "$(id -u)"
    else
        printf 'user/%s' "$(id -u)"
    fi
}

# launchd_bootstrap loads the plist into domain $1 from file $2, retrying
# while a previous bootout is still tearing the job down. Captures stderr
# in LAUNCHCTL_ERR for explain_bootstrap_error.
launchd_bootstrap() {
    _domain="$1"
    _plist="$2"
    # Plain sudo here (not run_sudo): its echo would land in LAUNCHCTL_ERR.
    _pfx=""
    if [ "$_domain" = system ]; then
        _pfx="sudo"
        info "  sudo launchctl bootstrap $_domain $_plist"
    fi

    # Clear a stale "disabled" override (an old `launchctl unload -w`).
    $_pfx launchctl enable "$_domain/$LAUNCHD_LABEL" >/dev/null 2>&1 || true

    _try=0
    while :; do
        if LAUNCHCTL_ERR="$($_pfx launchctl bootstrap "$_domain" "$_plist" 2>&1)"; then
            return 0
        fi
        case "$LAUNCHCTL_ERR" in
            *"already in progress"*|*"failed: 37:"*)
                _try=$((_try + 1))
                [ "$_try" -lt 10 ] || return 1
                sleep 1
                ;;
            *"failed: 5:"*|*"Input/output error"*)
                # "5: Input/output error" is launchd for "already loaded" (or a
                # plist it refuses). Loaded is fine.
                if launchctl print "$_domain/$LAUNCHD_LABEL" >/dev/null 2>&1; then
                    warn "Service was already loaded; reusing it"
                    return 0
                fi
                return 1
                ;;
            *) return 1 ;;
        esac
    done
}

explain_bootstrap_error() {
    case "$LAUNCHCTL_ERR" in
        *"failed: 125:"*|*"does not support"*|*"Could not find domain"*)
            print_no_gui_caveat
            ;;
        *"failed: 5:"*|*"Input/output error"*)
            warn "launchd rejected the plist. Check:  plutil -lint \"$1\"  and  test -x \"$INSTALL_DIR/hqsshd\""
            ;;
        *"failed: 133:"*|*"disabled"*)
            warn "The service is disabled in launchd. Run:  launchctl enable $2/$LAUNCHD_LABEL"
            ;;
        *"bad ownership"*|*"permissions"*)
            warn "The plist must be owned by you with mode 0644 (user scope) or root:wheel 0644 (system scope)."
            ;;
        *"Operation not permitted"*)
            warn "launchd refused the request from this context (a sandbox, or root without sudo -u?)."
            ;;
        *)
            warn "launchctl said: $LAUNCHCTL_ERR"
            ;;
    esac
}

start_service_launchd() {
    command -v launchctl >/dev/null 2>&1 || return 0

    if [ "$SERVICE_SCOPE" = system ]; then
        _domain=system
        _plist="$SYSTEM_PLIST"
        _pfx="run_sudo"
    else
        _plist="$LAUNCHD_PLIST"
        _pfx=""
        [ -f "$_plist" ] || return 0
        _domain="$(pick_agent_domain)"
        case "$_domain" in
            user/*)
                warn "No GUI login session for $(id -un); loading the agent into $_domain instead."
                warn "It runs while you have a session on this Mac and does not start at boot."
                print_no_gui_caveat
                ;;
        esac
    fi

    info "Loading $_domain/$LAUNCHD_LABEL..."
    if ! launchd_bootstrap "$_domain" "$_plist"; then
        warn "Bootstrap failed: $LAUNCHCTL_ERR"
        explain_bootstrap_error "$_plist" "$_domain"
        service_failed_launchd "$_domain"
    fi

    # RunAtLoad already started the job. Only nudge launchd if no pid shows
    # up — and never `kickstart -k` here: that would kill the process
    # bootstrap just spawned and start a second one.
    _i=0
    while [ "$_i" -lt 3 ] && [ -z "$(launchd_service_pid "$_domain")" ]; do
        sleep 1
        _i=$((_i + 1))
    done
    if [ -z "$(launchd_service_pid "$_domain")" ]; then
        info "No process yet — asking launchd to start it"
        $_pfx launchctl kickstart "$_domain/$LAUNCHD_LABEL" 2>/dev/null || true
    fi

    if wait_for_healthy 20; then
        info "Service started (pid $(launchd_service_pid "$_domain"), version $(running_version))"
    else
        service_failed_launchd "$_domain"
    fi
}

service_failed_launchd() {
    _domain="$1"
    printf '\n' >&2
    if have_doctor; then
        warn "The service did not pass its health check:"
        "$INSTALL_DIR/hqssh" doctor >&2 || true
        printf '\n' >&2
    fi
    warn "launchd says:"
    for _f in state pid runs "last exit code"; do
        _v="$(launchd_service_field "$_domain" "$_f")"
        [ -n "$_v" ] && warn "    $_f = $_v"
    done
    if [ -f "$LOG_FILE" ]; then
        warn "Last log lines ($LOG_FILE):"
        tail -n 30 "$LOG_FILE" >&2 || true
    fi
    printf '\n' >&2
    warn "Next steps:"
    warn "    $INSTALL_DIR/hqssh doctor"
    warn "    launchctl print $_domain/$LAUNCHD_LABEL"
    warn "    tail -f $LOG_FILE"
    exit 3
}

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

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
    _pid="$(service_pid_any | head -1)"
    printf '\n'
    info "${BOLD}Installation complete!${RESET}"
    printf '\n'
    if [ -n "$_pid" ] && [ "${HQSSH_NO_START:-}" != "1" ]; then
        printf '  Version:   %s (running, pid %s)\n' "$(running_version)" "$_pid"
    else
        printf '  Version:   %s\n' "$(binary_version "$INSTALL_DIR/hqsshd")"
    fi
    printf '  Binaries:  %s/hqsshd, %s/hqssh\n' "$INSTALL_DIR" "$INSTALL_DIR"

    _service_installed=0
    if [ "${HQSSH_NO_SERVICE:-}" != "1" ]; then
        if [ "$OS" = linux ] && command -v systemctl >/dev/null 2>&1; then
            printf '  Service:   %s (systemd user unit: %s)\n' "$SERVICE_NAME" "$SERVICE_FILE"
            printf '  Logs:      journalctl --user -u %s -f\n' "$SERVICE_NAME"
            _service_installed=1
        elif [ "$OS" = darwin ] && command -v launchctl >/dev/null 2>&1; then
            if [ "$SERVICE_SCOPE" = system ]; then
                printf '  Service:   %s (launchd system daemon: %s)\n' "$LAUNCHD_LABEL" "$SYSTEM_PLIST"
            else
                printf '  Service:   %s (launchd user agent: %s)\n' "$LAUNCHD_LABEL" "$LAUNCHD_PLIST"
            fi
            printf '  Logs:      %s\n' "$LOG_FILE"
            _service_installed=1
        fi
    fi

    printf '\n'
    if [ "$_service_installed" = 1 ]; then
        printf '  Manage the daemon:\n'
        if have_doctor; then
            printf '    hqssh doctor                                   # health check (works without the daemon)\n'
            printf '    hqssh service status|start|stop|restart|logs\n'
        else
            printf '    (hqssh >= v1.4.0 adds "hqssh doctor" and "hqssh service ...")\n'
        fi
        if [ "$OS" = linux ]; then
            printf '    manual: systemctl --user status|restart %s ; journalctl --user -u %s -f\n' "$SERVICE_NAME" "$SERVICE_NAME"
        elif [ "$SERVICE_SCOPE" = system ]; then
            printf '    manual: sudo launchctl print system/%s ; sudo launchctl kickstart -k system/%s\n' "$LAUNCHD_LABEL" "$LAUNCHD_LABEL"
        else
            # shellcheck disable=SC2016 # the $(id -u) is meant for the user's shell
            printf '    manual: launchctl print gui/$(id -u)/%s ; launchctl kickstart -k gui/$(id -u)/%s\n' "$LAUNCHD_LABEL" "$LAUNCHD_LABEL"
        fi
        if [ "$OS" = darwin ] && [ "$SERVICE_SCOPE" = user ]; then
            printf '\n'
            printf '  Note: a user agent starts when you log in to the Mac, not at boot. For a\n'
            printf '  headless Mac or unattended reboots, install the system daemon instead:\n'
            printf '    curl -fsSL https://hqssh.com/install | HQSSH_SERVICE_SCOPE=system sh\n'
            if [ -n "${SSH_CONNECTION:-}" ] || [ "$(console_user)" != "$(id -un)" ]; then
                printf '  (You appear to be installing remotely; the system daemon is probably what you want.)\n'
            fi
        fi
    elif [ "$OS" = darwin ]; then
        printf '  To start the daemon manually:\n'
        printf '    %s/hqsshd\n' "$INSTALL_DIR"
    fi
    printf '\n'
}

# ---------------------------------------------------------------------------
# Uninstall
# ---------------------------------------------------------------------------

uninstall() {
    info "Uninstalling hqsshd..."

    _os="$(uname -s | tr '[:upper:]' '[:lower:]')"
    OS="$_os"

    if [ "$_os" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
        systemd_env
        _svc_pid="$(systemctl --user show -p MainPID --value "$SERVICE_NAME" 2>/dev/null || true)"
        [ "${_svc_pid:-0}" = 0 ] && _svc_pid=""
        systemctl --user stop "$SERVICE_NAME" 2>/dev/null || true
        systemctl --user disable "$SERVICE_NAME" 2>/dev/null || true
        rm -f "$SERVICE_FILE"
        systemctl --user daemon-reload 2>/dev/null || true
    fi

    _svc_pid=""
    if [ "$_os" = "darwin" ] && command -v launchctl >/dev/null 2>&1; then
        _domain="$(launchd_find_domain)"
        [ -n "$_domain" ] && _svc_pid="$(launchd_service_pid "$_domain")"
        if [ -n "$_domain" ] && [ "$_domain" != system ]; then
            info "Unloading $_domain/$LAUNCHD_LABEL"
            launchctl bootout "$_domain/$LAUNCHD_LABEL" 2>/dev/null || true
        fi
        if [ "$_domain" = system ] || [ -f "$SYSTEM_PLIST" ]; then
            info "Unloading system/$LAUNCHD_LABEL (needs sudo)"
            ensure_sudo
            run_sudo launchctl bootout "system/$LAUNCHD_LABEL" || true
            run_sudo rm -f "$SYSTEM_PLIST"
        fi
        rm -f "$LAUNCHD_PLIST"
    fi

    # shellcheck disable=SC2046 # intentional word split: one pid per argument
    set -- $(daemon_pids_to_stop "$_svc_pid")
    if [ $# -gt 0 ]; then
        info "Stopping hqsshd (pid $*)"
        kill -TERM "$@" 2>/dev/null || true
        stop_daemon_processes "$@" || true
    fi

    rm -f "$INSTALL_DIR/hqsshd" "$INSTALL_DIR/hqssh" "$PIDFILE"
    if [ -z "$(running_daemon_pids)" ]; then
        rm -f "$SOCKET_PATH"
    fi

    if [ "$PURGE_LOGS" = 1 ]; then
        rm -rf "$LOG_DIR"
        _logs="logs removed"
    else
        _logs="logs kept in $LOG_DIR (remove with --purge-logs)"
    fi

    if [ $# -gt 0 ] && [ -n "$(alive_pids "$@")" ]; then
        warn "hqsshd is still running (pid $(alive_pids "$@" | tr '\n' ' ')); kill it by hand"
        exit 3
    fi
    info "Uninstalled: no hqsshd process, no plist/unit, no socket. Data in $DATA_DIR was preserved; $_logs."
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

usage() {
    printf 'Usage: install.sh [--system] [--uninstall [--purge-logs]] [--help]\n\n'
    printf 'Install or update hqsshd.\n\n'
    printf 'Options:\n'
    printf '  --system            macOS: install as a system daemon (starts at boot, uses sudo)\n'
    printf '  --uninstall         Stop the service and remove binaries and service definitions\n'
    printf '  --purge-logs        With --uninstall: also remove ~/.hqssh/logs\n\n'
    printf 'Environment variables:\n'
    printf '  HQSSH_VERSION        Pin a specific version (default: latest)\n'
    printf '  HQSSH_INSTALL_DIR    Override install directory (default: ~/.local/bin)\n'
    printf '  HQSSH_SERVICE_SCOPE  user (default) | system (macOS only)\n'
    printf '  HQSSH_NO_SERVICE     Set to 1 to skip systemd/launchd service setup\n'
    printf '  HQSSH_NO_START       Set to 1 to skip starting the service\n'
    printf '  HQSSH_LOCAL_ARCHIVE  Install from a local hqsshd-<os>-<arch>.tar.gz\n\n'
    printf 'Exit codes: 0 ok, 1 error (nothing installed), 2 usage, 3 service did not start\n'
}

main() {
    _do_uninstall=0
    for arg in "$@"; do
        case "$arg" in
            --uninstall) _do_uninstall=1 ;;
            --purge-logs) PURGE_LOGS=1 ;;
            --system) SERVICE_SCOPE=system ;;
            --help|-h) usage; exit 0 ;;
            *) usage >&2; printf '\nUnknown option: %s\n' "$arg" >&2; exit 2 ;;
        esac
    done

    if [ "$_do_uninstall" = 1 ]; then
        uninstall
        exit 0
    fi

    if [ -n "${HQSSH_LOCAL_ARCHIVE:-}" ]; then
        LOCAL_ARCHIVE=1
    fi

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
