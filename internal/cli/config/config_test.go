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

	t.Setenv("HQSSH_DAEMON_PORT", "50060")
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
