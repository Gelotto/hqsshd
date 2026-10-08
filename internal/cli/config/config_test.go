package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".hqssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	globalConfig = nil // Load() caches
	t.Cleanup(func() { globalConfig = nil })
}

func TestResolveDaemonPortPrecedence(t *testing.T) {
	writeConfig(t, `
default_host: box
hosts:
  box:
    host: box.example.com
    user: u
    daemon_port: 50052
  plain:
    host: plain.example.com
`)
	t.Setenv("HQSSH_DAEMON_PORT", "")

	r := Resolve("", "", "", "", 0, 0)
	if r.Host != "box.example.com" || r.DaemonPort != 50052 {
		t.Errorf("default host: %+v", r)
	}

	r = Resolve("plain", "", "", "", 0, 0)
	if r.DaemonPort != DefaultDaemonPort {
		t.Errorf("host without daemon_port: got %d, want default %d", r.DaemonPort, DefaultDaemonPort)
	}

	// A raw -H host that is not an alias must not inherit the default
	// host's daemon_port
	r = Resolve("other.example.com", "", "", "", 0, 0)
	if r.Host != "other.example.com" || r.DaemonPort != DefaultDaemonPort {
		t.Errorf("raw host: got %s:%d, want default daemon port %d", r.Host, r.DaemonPort, DefaultDaemonPort)
	}

	t.Setenv("HQSSH_DAEMON_PORT", "50060")
	r = Resolve("other.example.com", "", "", "", 0, 0)
	if r.DaemonPort != 50060 {
		t.Errorf("raw host should use HQSSH_DAEMON_PORT: %d", r.DaemonPort)
	}
	r = Resolve("", "", "", "", 0, 0)
	if r.DaemonPort != 50060 {
		t.Errorf("env should override the config file: %d", r.DaemonPort)
	}
	r = Resolve("box", "", "", "", 0, 0)
	if r.DaemonPort != 50052 {
		t.Errorf("an explicit alias keeps its own daemon_port: %d", r.DaemonPort)
	}

	r = Resolve("box", "", "", "", 0, 50070)
	if r.DaemonPort != 50070 {
		t.Errorf("--daemon-port must win: %d", r.DaemonPort)
	}
}

func TestResolveAuthTokenStaysWithItsHost(t *testing.T) {
	writeConfig(t, `
default_host: box
hosts:
  box:
    host: box.example.com
    auth_token: box-secret
  other:
    host: other.example.com
    auth_token: other-secret
  plain:
    host: plain.example.com
`)
	t.Setenv("HQSSH_AUTH_TOKEN", "")
	t.Setenv("HQSSH_HOST", "")

	for _, tc := range []struct {
		cliHost, want string
	}{
		{"", "box-secret"},
		{"box", "box-secret"},
		{"other", "other-secret"},
		{"plain", ""},           // alias without a token
		{"raw.example.com", ""}, // not an alias: never the default host's token
	} {
		if got := Resolve(tc.cliHost, "", "", "", 0, 0).AuthToken; got != tc.want {
			t.Errorf("Resolve(%q).AuthToken = %q, want %q", tc.cliHost, got, tc.want)
		}
	}

	t.Setenv("HQSSH_HOST", "env.example.com")
	if got := Resolve("", "", "", "", 0, 0).AuthToken; got != "" {
		t.Errorf("HQSSH_HOST pointing elsewhere kept the default host's token %q", got)
	}
	t.Setenv("HQSSH_HOST", "")

	t.Setenv("HQSSH_AUTH_TOKEN", "env-secret")
	for _, tc := range []struct {
		cliHost, want string
	}{
		{"", "env-secret"},
		{"other", "other-secret"}, // a per-host token is more specific
		{"plain", "env-secret"},
		{"raw.example.com", "env-secret"},
	} {
		if got := Resolve(tc.cliHost, "", "", "", 0, 0).AuthToken; got != tc.want {
			t.Errorf("with HQSSH_AUTH_TOKEN: Resolve(%q).AuthToken = %q, want %q", tc.cliHost, got, tc.want)
		}
	}
}
