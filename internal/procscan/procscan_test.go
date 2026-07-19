// Copyright 2024 Gelotto
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package procscan

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var testTools = map[string]bool{"claude": true, "codex": true, "aider": true}

func TestToolFromArgv(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{"empty", nil, ""},
		{"bare tool", []string{"claude"}, "claude"},
		{"absolute path", []string{"/usr/local/bin/claude", "--continue"}, "claude"},
		{"home path", []string{"/home/u/.local/bin/codex"}, "codex"},
		{"node shim", []string{"node", "/usr/local/bin/claude"}, "claude"},
		{"node shim with flags", []string{"node", "--max-old-space-size=4096", "/usr/bin/claude"}, "claude"},
		{"python entry point", []string{"python3", "/home/u/.local/bin/aider", "--model", "gpt-4"}, "aider"},
		{"bun shim", []string{"bun", "/opt/bin/claude"}, "claude"},
		{"shell wrapper is not a tool", []string{"zsh", "-l", "-i", "-c", "claude"}, ""},
		{"tool name in later args only", []string{"grep", "claude"}, ""},
		{"interpreter script is not a tool", []string{"node", "/srv/app/server.js", "claude"}, ""},
		{"similar name", []string{"claude-monitor"}, ""},
		{"interpreter alone", []string{"python3"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolFromArgv(tt.argv, testTools); got != tt.want {
				t.Errorf("toolFromArgv(%v) = %q, want %q", tt.argv, got, tt.want)
			}
		})
	}
}

func TestDisplayCommand(t *testing.T) {
	// Under the limit: joined verbatim
	if got := displayCommand([]string{"claude", "--continue"}); got != "claude --continue" {
		t.Errorf("got %q", got)
	}

	// Truncation lands on a rune boundary even when a multi-byte rune
	// straddles the cut (proto rejects invalid UTF-8)
	long := strings.Repeat("a", maxCommandDisplay-1) + "🐛🐛🐛"
	got := displayCommand([]string{long})
	if len(got) > maxCommandDisplay {
		t.Errorf("length %d exceeds limit", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncated command is not valid UTF-8: %q", got)
	}

	// Raw invalid bytes from cmdline are coerced to valid UTF-8
	if got := displayCommand([]string{"claude", "\xff\xfe"}); !utf8.ValidString(got) {
		t.Errorf("invalid bytes not sanitized: %q", got)
	}
}

func TestParseStatData(t *testing.T) {
	// comm containing spaces and parentheses must not break field offsets
	stat := "55648 (my (weird) comm) S 55565 55648 55565 34816 55648 4194304 " +
		"100 0 0 0 5 3 0 0 20 0 8 0 123456 1000000 500 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0"

	ppid, starttime, err := parseStatData(stat)
	if err != nil {
		t.Fatalf("parseStatData: %v", err)
	}
	if ppid != 55565 {
		t.Errorf("ppid = %d, want 55565", ppid)
	}
	if starttime != 123456 {
		t.Errorf("starttime = %d, want 123456", starttime)
	}

	if _, _, err := parseStatData("garbage with no paren"); err == nil {
		t.Error("expected error for malformed stat")
	}
}

func TestIsDescendant(t *testing.T) {
	// 100 -> 50 -> 10 -> 1
	ppids := map[int]int{100: 50, 50: 10, 10: 1}

	if !isDescendant(100, map[int]bool{10: true}, ppids) {
		t.Error("100 should be a descendant of 10")
	}
	if isDescendant(100, map[int]bool{99: true}, ppids) {
		t.Error("100 should not be a descendant of 99")
	}
	// Direct root pid itself is not its own descendant
	if isDescendant(10, map[int]bool{10: true}, ppids) {
		t.Error("a root is not its own descendant")
	}
	// Cycle from pid reuse must terminate
	cyclic := map[int]int{5: 6, 6: 5}
	if isDescendant(5, map[int]bool{99: true}, cyclic) {
		t.Error("cycle should not match")
	}
}

// TestScanAndKill spawns a real process under an isolated tool name (a real
// AI tool name would trip the same-tool ancestor dedupe when the test itself
// runs inside a claude session) and verifies it is discovered with the right
// cwd, excluded when the test process is an exclusion root, and terminated
// by Kill.
func TestScanAndKill(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procscan requires /proc")
	}

	// Copy /bin/sleep to <tmp>/hqfaketool so argv[0] basename matches
	dir := t.TempDir()
	fake := filepath.Join(dir, "hqfaketool")
	src, err := os.Open("/bin/sleep")
	if err != nil {
		t.Skipf("no /bin/sleep: %v", err)
	}
	dst, err := os.OpenFile(fake, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	src.Close()
	dst.Close()

	cmd := exec.Command(fake, "60")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	pid := cmd.Process.Pid

	find := func(sessions []ExternalSession) *ExternalSession {
		for i := range sessions {
			if sessions[i].PID == pid {
				return &sessions[i]
			}
		}
		return nil
	}

	found := find(Scan([]string{"hqfaketool", "codex"}, nil))
	if found == nil {
		t.Fatalf("fake tool (pid %d) not found by Scan", pid)
	}
	if found.Tool != "hqfaketool" {
		t.Errorf("tool = %q, want hqfaketool", found.Tool)
	}
	// cwd may traverse symlinks (macOS-style /tmp); resolve both sides
	wantDir, _ := filepath.EvalSymlinks(dir)
	gotDir, _ := filepath.EvalSymlinks(found.WorkingDir)
	if gotDir != wantDir {
		t.Errorf("cwd = %q, want %q", gotDir, wantDir)
	}
	if found.StartedAt.IsZero() || time.Since(found.StartedAt) > time.Minute {
		t.Errorf("implausible StartedAt: %v", found.StartedAt)
	}

	// Excluding this test process's pid must hide its child
	if find(Scan([]string{"hqfaketool"}, []int{os.Getpid()})) != nil {
		t.Error("descendant of exclusion root should be skipped")
	}

	// Kill with the wrong tool must refuse
	if err := Kill(pid, "codex", []string{"hqfaketool", "codex"}); err == nil {
		t.Error("Kill with mismatched tool should fail")
	}

	// Kill with the right tool terminates it
	if err := Kill(pid, "hqfaketool", []string{"hqfaketool"}); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		// Terminated as expected
	case <-time.After(2 * time.Second):
		t.Error("process still alive 2s after Kill")
	}
}
