package client

// Review-only tests (not part of the repo).

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// A passphrase-protected key given with -k, when the passphrase cannot be
// read (stdin is not a TTY under tests), is silently dropped and the user
// is told "no authentication methods available ... Tried default keys".
func TestPassphraseKeyErrorIsSwallowed(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "id_ed25519")
	cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "hunter2", "-f", key)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ssh-keygen unavailable: %v %s", err, out)
	}
	// make sure no default keys are picked up
	t.Setenv("HOME", dir)

	// stdin is not a terminal here, so promptPassphrase fails
	_, err := buildSSHConfig(Config{Host: "example.invalid", Port: 22, User: "u", KeyPath: key})
	if err == nil {
		t.Fatal("expected an error")
	}
	t.Logf("error shown to the user: %q", err.Error())
	if !strings.Contains(err.Error(), key) && !strings.Contains(strings.ToLower(err.Error()), "passphrase") {
		t.Errorf("error does not mention the key or the passphrase problem; it blames missing default keys instead")
	}
}

// Hashed known_hosts entries are supported by x/crypto knownhosts; a
// changed key is reported as "key mismatch" (which the client maps to the
// MITM warning and treats as permanent).
func TestHashedKnownHostsAndChangedKey(t *testing.T) {
	pub1, _, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	k1, _ := ssh.NewPublicKey(pub1)
	k2, _ := ssh.NewPublicKey(pub2)

	line := knownhosts.Line([]string{knownhosts.HashHostname("example.com")}, k1)
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cb, err := knownhosts.New(path)
	if err != nil {
		t.Fatal(err)
	}
	addr := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 22}
	if err := cb("example.com:22", addr, k1); err != nil {
		t.Errorf("hashed entry not matched: %v", err)
	}
	err = cb("example.com:22", addr, k2)
	if err == nil {
		t.Fatal("changed host key accepted")
	}
	t.Logf("changed key error: %v", err)
	if !contains(err.Error(), "key mismatch") {
		t.Errorf("client.go matches on 'key mismatch' but error is %q", err.Error())
	}
	if !isPermanentError(err.Error()) {
		t.Errorf("changed key error is retried (not permanent)")
	}
	// unknown host
	err = cb("other.example.com:22", addr, k1)
	if err == nil || !contains(err.Error(), "key is unknown") {
		t.Errorf("unknown host error = %v", err)
	}
}

// Loopback hosts skip host-key verification without any warning.
func TestLoopbackSkipsHostKeyCheck(t *testing.T) {
	for _, h := range []string{"localhost", "127.0.0.1", "::1"} {
		if !isLoopback(h) {
			t.Errorf("%s not loopback", h)
		}
	}
	t.Logf("note: -H localhost/127.0.0.1 uses ssh.InsecureIgnoreHostKey() with no warning (client.go buildSSHConfig)")
}
