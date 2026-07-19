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
// outside daemon management by scanning /proc. Such "external sessions" were
// started from a regular terminal (e.g. an SSH shell), so they cannot be
// attached to, but they can be listed per project and terminated.
package procscan

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

// userHZ is the kernel clock tick rate used for /proc starttime. Linux has
// reported 100 to userspace on all mainstream architectures for decades.
const userHZ = 100

// Scan returns external AI tool processes visible in /proc. Processes that
// are descendants of any pid in excludeRoots (typically the daemon itself,
// so daemon-managed sessions aren't double-reported) are skipped, as are
// processes whose cwd can't be read (other users' processes).
func Scan(tools []string, excludeRoots []int) []ExternalSession {
	toolSet := make(map[string]bool, len(tools))
	for _, t := range tools {
		toolSet[t] = true
	}
	rootSet := make(map[int]bool, len(excludeRoots))
	for _, p := range excludeRoots {
		rootSet[p] = true
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		// Non-Linux or /proc unavailable - nothing to report
		return nil
	}

	// First pass: pid -> ppid for ancestry checks, pid -> starttime.
	ppids := make(map[int]int)
	starts := make(map[int]uint64)
	pids := make([]int, 0, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		ppid, starttime, err := parseStat(pid)
		if err != nil {
			continue // Process vanished mid-scan
		}
		ppids[pid] = ppid
		starts[pid] = starttime
		pids = append(pids, pid)
	}

	boot := bootTime()

	// Second pass: match tools by cmdline. Kept separate so parent/child
	// pairs that both match (e.g. a "node .../codex" shim and the native
	// binary it spawns) can be collapsed to the top-most process below.
	matched := make(map[int]string)
	argvs := make(map[int][]string)
	for _, pid := range pids {
		argv, err := readCmdline(pid)
		if err != nil || len(argv) == 0 {
			continue // Vanished or kernel thread
		}
		if tool := toolFromArgv(argv, toolSet); tool != "" {
			matched[pid] = tool
			argvs[pid] = argv
		}
	}

	var result []ExternalSession
	for _, pid := range pids {
		tool, ok := matched[pid]
		if !ok {
			continue
		}
		argv := argvs[pid]

		if isDescendant(pid, rootSet, ppids) {
			continue // Daemon-managed session (or other excluded subtree)
		}

		if hasMatchedAncestor(pid, tool, matched, ppids) {
			continue // Child of the same logical session (shim -> binary)
		}

		cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
		if err != nil {
			continue // Not our process (EACCES) or vanished
		}

		var startedAt time.Time
		if !boot.IsZero() {
			// Divide ticks first: ticks * time.Second overflows int64 once
			// uptime passes ~2.9 years. Sub-second precision is irrelevant.
			startedAt = boot.Add(time.Duration(starts[pid]/userHZ) * time.Second)
		}

		result = append(result, ExternalSession{
			PID:        pid,
			Tool:       tool,
			WorkingDir: strings.ToValidUTF8(cwd, "�"),
			StartedAt:  startedAt,
			Command:    displayCommand(argv),
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
	argv, err := readCmdline(pid)
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

// readCmdline reads a process's NUL-separated argv.
func readCmdline(pid int) ([]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	if len(parts) == 1 && parts[0] == "" {
		return nil, nil
	}
	return parts, nil
}

// parseStat extracts ppid and starttime (clock ticks since boot) from
// /proc/<pid>/stat. The comm field may contain spaces and parentheses, so
// fields are located relative to the LAST ')'.
func parseStat(pid int) (ppid int, starttime uint64, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, err
	}
	return parseStatData(string(data))
}

func parseStatData(data string) (ppid int, starttime uint64, err error) {
	end := strings.LastIndexByte(data, ')')
	if end < 0 {
		return 0, 0, fmt.Errorf("malformed stat")
	}
	// After ")": state(0) ppid(1) pgrp(2) session(3) tty(4) tpgid(5) flags(6)
	// minflt(7) cminflt(8) majflt(9) cmajflt(10) utime(11) stime(12)
	// cutime(13) cstime(14) priority(15) nice(16) threads(17) itreal(18)
	// starttime(19)
	fields := strings.Fields(data[end+1:])
	if len(fields) < 20 {
		return 0, 0, fmt.Errorf("malformed stat: %d fields", len(fields))
	}
	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("malformed ppid: %w", err)
	}
	starttime, err = strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("malformed starttime: %w", err)
	}
	return ppid, starttime, nil
}

// bootTime returns the system boot time from /proc/stat, or zero if unknown.
func bootTime() time.Time {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			secs, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err != nil {
				return time.Time{}
			}
			return time.Unix(secs, 0)
		}
	}
	return time.Time{}
}
