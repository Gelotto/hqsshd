# Security Policy

## Supported Versions

| Version | Supported          |
|---------|--------------------|
| latest  | :white_check_mark: |

## Reporting a Vulnerability

If you discover a security vulnerability in hqsshd, please report it responsibly.

**Do NOT open a public GitHub issue for security vulnerabilities.**

Instead, please email: **admin@gelotto.io**

Include:
- Description of the vulnerability
- Steps to reproduce
- Impact assessment
- Suggested fix (if any)

## Response Timeline

- **Acknowledgment**: Within 48 hours
- **Initial assessment**: Within 1 week
- **Fix or mitigation**: Depends on severity, targeting 30 days for critical issues

## Security Model

hqsshd is designed to run as a **user-level daemon** on remote systems accessed via SSH. Key security properties:

- **Listening scope**: Unix socket (owner-only permissions `0600`) and TCP on `127.0.0.1` only (localhost, not exposed to network)
- **Authentication**: Optional token-based auth with constant-time comparison
- **Process isolation**: Systemd hardening with `NoNewPrivileges`, `ProtectSystem=strict`, `PrivateTmp`
- **Tool execution**: Tool names validated against a configurable whitelist; shell tool requires explicit opt-in
- **Command injection prevention**: User prompts passed as positional arguments (`$1`), not interpolated into shell commands

## Scope

The following are in scope for security reports:
- Authentication bypass
- Command injection or arbitrary code execution
- Privilege escalation
- Information disclosure
- Denial of service via crafted gRPC messages

Out of scope:
- Issues requiring physical access to the host
- Issues in dependencies (report upstream, but let us know)
- Social engineering
