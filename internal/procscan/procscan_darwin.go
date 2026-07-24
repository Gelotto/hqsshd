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

//go:build darwin

package procscan

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// lsofTimeout bounds the batched cwd lookup so a wedged lsof can't hang the
// ListExternalSessions RPC.
const lsofTimeout = 5 * time.Second

// listProcesses enumerates the kernel process table via sysctl. argv comes
// from KERN_PROCARGS2, which the kernel only serves for the caller's own
// processes - other users' processes get nil argv and are never matched,
// mirroring the /proc EACCES behavior on Linux.
func listProcesses() map[int]procInfo {
	kprocs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}

	procs := make(map[int]procInfo, len(kprocs))
	for i := range kprocs {
		kp := &kprocs[i]
		pid := int(kp.Proc.P_pid)
		if pid <= 0 {
			continue
		}
		var startedAt time.Time
		if sec := kp.Proc.P_starttime.Sec; sec > 0 {
			startedAt = time.Unix(sec, int64(kp.Proc.P_starttime.Usec)*1000)
		}
		argv, err := readArgv(pid)
		if err != nil {
			argv = nil // Other user's process, zombie, or vanished mid-scan
		}
		procs[pid] = procInfo{
			ppid:      int(kp.Eproc.Ppid),
			startedAt: startedAt,
			argv:      argv,
		}
	}
	return procs
}

// readArgv reads a process's argv via the KERN_PROCARGS2 sysctl.
func readArgv(pid int) ([]string, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	return parseProcArgs2(buf)
}

// parseProcArgs2 decodes a KERN_PROCARGS2 buffer: a native-endian int32
// argc, the executable path (NUL-terminated, then NUL padding to an aligned
// boundary), then argc NUL-terminated argv strings, then the environment.
func parseProcArgs2(buf []byte) ([]string, error) {
	if len(buf) < 4 {
		return nil, fmt.Errorf("procargs2 buffer too short: %d bytes", len(buf))
	}
	// Darwin runs little-endian on every supported architecture.
	argc := int(int32(binary.LittleEndian.Uint32(buf[:4])))
	if argc <= 0 {
		return nil, fmt.Errorf("procargs2 argc = %d", argc)
	}
	rest := buf[4:]

	// Skip the exec path and its NUL padding.
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, fmt.Errorf("procargs2 missing exec path terminator")
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}

	argv := make([]string, 0, argc)
	for len(argv) < argc && len(rest) > 0 {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			argv = append(argv, string(rest))
			break
		}
		argv = append(argv, string(rest[:i]))
		rest = rest[i+1:]
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("procargs2 yielded no argv")
	}
	return argv, nil
}

// cwdFor resolves working directories with a single batched lsof call
// (lsof ships with macOS; there is no sysctl for another process's cwd and
// proc_pidinfo has no Go binding). Only the handful of matched candidate
// pids reach here, so the exec cost is paid rarely and once per scan. Pids
// lsof reports nothing for (exited, or not visible to this user) are
// omitted, matching the Linux readlink-EACCES behavior.
func cwdFor(pids []int) map[int]string {
	if len(pids) == 0 {
		return nil
	}

	parts := make([]string, len(pids))
	for i, pid := range pids {
		parts[i] = strconv.Itoa(pid)
	}

	ctx, cancel := context.WithTimeout(context.Background(), lsofTimeout)
	defer cancel()

	// -a: AND the selectors; -d cwd: only the cwd descriptor; -Fn: machine-
	// readable output (p<pid> / n<path> lines); -w: suppress warnings.
	out, err := exec.CommandContext(ctx, "lsof", "-a", "-d", "cwd", "-Fn",
		"-w", "-p", strings.Join(parts, ",")).Output()
	// lsof exits non-zero when any requested pid yields no output; parse
	// whatever it did print rather than failing the whole scan.
	if len(out) == 0 && err != nil {
		return nil
	}
	return parseLsofCwd(out)
}

// parseLsofCwd decodes `lsof -Fn` field output: a `p<pid>` line introduces a
// process, and the following `n<path>` line carries its cwd.
func parseLsofCwd(out []byte) map[int]string {
	cwds := make(map[int]string)
	cur := 0
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid, err := strconv.Atoi(line[1:])
			if err != nil {
				cur = 0
				continue
			}
			cur = pid
		case strings.HasPrefix(line, "n") && cur > 0:
			cwds[cur] = line[1:]
		}
	}
	return cwds
}
