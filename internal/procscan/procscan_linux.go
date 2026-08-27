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

//go:build linux

package procscan

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// userHZ is the kernel clock tick rate used for /proc starttime. Linux has
// reported 100 to userspace on all mainstream architectures for decades.
const userHZ = 100

// listProcesses enumerates /proc. Processes whose stat or cmdline can't be
// read (vanished mid-scan, kernel threads) get nil argv or are dropped.
func listProcesses() map[int]procInfo {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}

	boot := bootTime()

	procs := make(map[int]procInfo, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		ppid, starttime, err := parseStat(pid)
		if err != nil {
			continue // Process vanished mid-scan
		}
		var startedAt time.Time
		if !boot.IsZero() {
			// Divide ticks first: ticks * time.Second overflows int64 once
			// uptime passes ~2.9 years. Sub-second precision is irrelevant.
			startedAt = boot.Add(time.Duration(starttime/userHZ) * time.Second)
		}
		argv, err := readArgv(pid)
		if err != nil {
			argv = nil // Vanished or kernel thread
		}
		procs[pid] = procInfo{ppid: ppid, startedAt: startedAt, argv: argv}
	}
	return procs
}

// readArgv reads a process's NUL-separated argv from /proc.
func readArgv(pid int) ([]string, error) {
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

// cwdFor resolves each pid's working directory via /proc/<pid>/cwd. Pids
// whose cwd can't be read (EACCES for other users' processes, or vanished)
// are omitted.
func cwdFor(pids []int) map[int]string {
	cwds := make(map[int]string, len(pids))
	for _, pid := range pids {
		cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
		if err != nil {
			continue
		}
		cwds[pid] = cwd
	}
	return cwds
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
// LsofPath is unused on Linux (cwd comes from /proc).
func LsofPath() string { return "" }

// BootTime returns when the machine booted (the btime line of /proc/stat).
func BootTime() (time.Time, bool) {
	bt := bootTime()
	return bt, !bt.IsZero()
}

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
