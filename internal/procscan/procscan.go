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

// Package procscan discovers AI CLI processes (claude, codex, aider) running
// outside daemon management. Such "external sessions" were started from a
// regular terminal (e.g. an SSH shell), so they cannot be attached to, but
// they can be listed per project and terminated.
//
// Process enumeration is platform-specific: /proc on Linux
// (procscan_linux.go), sysctl + lsof on macOS (procscan_darwin.go). Each
// platform file implements listProcesses, readArgv, and cwdFor; everything
// else is shared.
package procscan

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/gelotto/hqsshd/internal/logging"
)

// ExternalSession is an AI CLI process running outside daemon management.
type ExternalSession struct {
	PID        int
	Tool       string
	WorkingDir string
	StartedAt  time.Time // Zero if unknown
	Command    string    // Display command line (truncated)
}

// procInfo is one process from the platform's process table.
type procInfo struct {
	ppid      int
	startedAt time.Time // Zero if unknown
	argv      []string  // nil if unreadable (other user, kernel thread, vanished)
}

// interpreters whose first script argument names the real tool
// (e.g. "python3 ~/.local/bin/aider", "node /usr/local/bin/claude").
var interpreters = map[string]bool{
	"node":    true,
	"nodejs":  true,
	"bun":     true,
	"deno":    true,
	"python":  true,
	"python2": true,
	"python3": true,
}

// maxCommandDisplay bounds the Command field so huge argv lists don't bloat
// list responses.
const maxCommandDisplay = 160

// killGracePeriod is how long an external process gets to exit after SIGTERM
// before it is SIGKILLed.
const killGracePeriod = 3 * time.Second

// Scan returns external AI tool processes. Processes that are descendants of
// any pid in excludeRoots (typically the daemon itself, so daemon-managed
// sessions aren't double-reported) are skipped, as are processes whose cwd
// can't be read (other users' processes).
func Scan(tools []string, excludeRoots []int) []ExternalSession {
	toolSet := make(map[string]bool, len(tools))
	for _, t := range tools {
		toolSet[t] = true
	}
	rootSet := make(map[int]bool, len(excludeRoots))
	for _, p := range excludeRoots {
		rootSet[p] = true
	}

	procs := listProcesses()
	if len(procs) == 0 {
		return nil
	}

	ppids := make(map[int]int, len(procs))
	for pid, p := range procs {
		ppids[pid] = p.ppid
	}

	// Match tools by argv. Kept separate from filtering so parent/child
	// pairs that both match (e.g. a "node .../codex" shim and the native
	// binary it spawns) can be collapsed to the top-most process below.
	matched := make(map[int]string)
	for pid, p := range procs {
		if len(p.argv) == 0 {
			continue
		}
		if tool := toolFromArgv(p.argv, toolSet); tool != "" {
			matched[pid] = tool
		}
	}

	var candidates []int
	for pid, tool := range matched {
		if isDescendant(pid, rootSet, ppids) {
			continue // Daemon-managed session (or other excluded subtree)
		}
		if hasMatchedAncestor(pid, tool, matched, ppids) {
			continue // Child of the same logical session (shim -> binary)
		}
		candidates = append(candidates, pid)
	}

	cwds := cwdFor(candidates)

	var result []ExternalSession
	for _, pid := range candidates {
		cwd, ok := cwds[pid]
		if !ok {
			continue // Not our process or vanished mid-scan
		}
		result = append(result, ExternalSession{
			PID:        pid,
			Tool:       matched[pid],
			WorkingDir: strings.ToValidUTF8(cwd, "�"),
			StartedAt:  procs[pid].startedAt,
			Command:    displayCommand(procs[pid].argv),
		})
	}

	// Stable order: newest first (matches how session lists read elsewhere)
	sort.Slice(result, func(i, j int) bool {
		if !result[i].StartedAt.Equal(result[j].StartedAt) {
			return result[i].StartedAt.After(result[j].StartedAt)
		}
		return result[i].PID < result[j].PID
	})

	return result
}

// Kill terminates an external process after verifying it still matches the
// claimed tool (guards against PID reuse). SIGTERM first; a goroutine
// escalates to SIGKILL after a grace period if the same tool process
// still holds the PID. The signal targets only the pid, never its process
// group - an external session may share a group with the user's shell.
func Kill(pid int, tool string, tools []string) error {
	if pid <= 1 {
		return fmt.Errorf("invalid pid: %d", pid)
	}
	toolSet := make(map[string]bool, len(tools))
	for _, t := range tools {
		toolSet[t] = true
	}
	if !toolSet[tool] {
		return fmt.Errorf("unknown tool: %q", tool)
	}

	if err := verifyTool(pid, tool, toolSet); err != nil {
		return err
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("failed to signal process %d: %w", pid, err)
	}

	logging.Info("terminating external session", "pid", pid, "tool", tool)

	go func() {
		time.Sleep(killGracePeriod)
		// Re-verify identity before SIGKILL: the pid may have exited and
		// been reused by an unrelated process during the grace period.
		if verifyTool(pid, tool, toolSet) == nil {
			if err := syscall.Kill(pid, syscall.SIGKILL); err == nil {
				logging.Info("external session ignored SIGTERM, escalated to SIGKILL",
					"pid", pid, "tool", tool)
			}
		}
	}()

	return nil
}

// displayCommand renders argv for display: bounded length, truncated on a
// rune boundary, coerced to valid UTF-8. Proto string fields reject invalid
// UTF-8, and raw cmdline bytes (or a mid-rune cut) would fail the whole
// list response at marshal time.
func displayCommand(argv []string) string {
	cmd := strings.Join(argv, " ")
	if len(cmd) > maxCommandDisplay {
		cut := maxCommandDisplay
		for cut > 0 && !utf8.RuneStart(cmd[cut]) {
			cut--
		}
		cmd = cmd[:cut]
	}
	return strings.ToValidUTF8(cmd, "�")
}

// verifyTool checks that pid is currently running the given tool.
func verifyTool(pid int, tool string, toolSet map[string]bool) error {
	argv, err := readArgv(pid)
	if err != nil {
		return fmt.Errorf("process %d not found", pid)
	}
	if toolFromArgv(argv, toolSet) != tool {
		return fmt.Errorf("process %d is not a %s process", pid, tool)
	}
	return nil
}

// toolFromArgv returns which known tool a process's argv belongs to, or "".
// Matches the basename of argv[0] directly, or - when argv[0] is a script
// interpreter (node, python, ...) - the basename of the first non-flag
// argument (the script path, e.g. an npm bin shim or pip entry point).
func toolFromArgv(argv []string, toolSet map[string]bool) string {
	if len(argv) == 0 {
		return ""
	}
	base := filepath.Base(argv[0])
	if toolSet[base] {
		return base
	}
	if !interpreters[base] {
		return ""
	}
	for _, arg := range argv[1:] {
		if strings.HasPrefix(arg, "-") {
			continue // Interpreter flag (e.g. --max-old-space-size=...)
		}
		script := filepath.Base(arg)
		if toolSet[script] {
			return script
		}
		return "" // First positional arg is the script; anything after is its args
	}
	return ""
}

// hasMatchedAncestor reports whether pid has an ancestor that matched the
// same tool. That ancestor is the process the user actually launched (e.g.
// an npm shim), so only it is reported.
func hasMatchedAncestor(pid int, tool string, matched map[int]string, ppids map[int]int) bool {
	for depth := 0; depth < 128; depth++ {
		ppid, ok := ppids[pid]
		if !ok || ppid <= 1 {
			return false
		}
		if matched[ppid] == tool {
			return true
		}
		pid = ppid
	}
	return false
}

// isDescendant reports whether pid has any ancestor in roots. The ppid map
// snapshot may be slightly stale, which at worst mis-skips a single scan.
func isDescendant(pid int, roots map[int]bool, ppids map[int]int) bool {
	if len(roots) == 0 {
		return false
	}
	// Depth cap guards against ppid cycles from pid reuse mid-scan.
	for depth := 0; depth < 128; depth++ {
		ppid, ok := ppids[pid]
		if !ok || ppid <= 1 {
			return false
		}
		if roots[ppid] {
			return true
		}
		pid = ppid
	}
	return false
}
