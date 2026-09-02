package session

import (
	"strings"
	"testing"
)

// COLUMNS/LINES must not reach the child: programs that honour them
// (Python shutil, ncurses use_env) would freeze at the creation size and
// ignore every later Resize. The PTY winsize + SIGWINCH is authoritative.
func TestBuildCommandDoesNotExportColumnsLines(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	t.Setenv("LINES", "24")
	s := NewSession("", "shell", "/tmp", "t", nil, 120, 40, 0)
	cmd, err := s.buildCommand()
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "COLUMNS=") || strings.HasPrefix(kv, "LINES=") {
			t.Errorf("child env carries %s", kv)
		}
	}
	found := false
	for _, kv := range cmd.Env {
		if kv == "TERM=xterm-256color" {
			found = true
		}
	}
	if !found {
		t.Error("TERM not set for the child")
	}
}
