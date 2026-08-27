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

package servicemgr

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// LaunchdStatus is launchd's view of the job, parsed from `launchctl print
// <domain>/<label>`.
type LaunchdStatus struct {
	Loaded      bool   // the job exists in the domain
	DomainFound bool   // false when the domain itself does not exist (no GUI login)
	State       string // "running", "not running", "spawn scheduled", ...
	PID         int
	Runs        int
	LastExit    string // raw: "(never exited)", "0", "1", "-9 (signal)"...
	ExitTimeout int    // seconds launchd waits after SIGTERM before SIGKILL
	Path        string // plist path
	Program     string
	StdoutPath  string
	StderrPath  string
	Raw         map[string]string // every top-level "key = value"
	Stderr      string            // launchctl's stderr when the print failed
}

// ParseLaunchctlPrint parses `launchctl print` output. Only one-tab-indented
// "key = value" lines inside the top-level block are read; nested blocks
// (which contain a misleading "state = active" for the resource coalition)
// are skipped.
func ParseLaunchctlPrint(stdout, stderr string) LaunchdStatus {
	st := LaunchdStatus{Raw: map[string]string{}, Stderr: strings.TrimSpace(stderr), DomainFound: true}

	if strings.TrimSpace(stdout) == "" {
		lower := strings.ToLower(stderr)
		if strings.Contains(lower, "could not find domain") || strings.Contains(lower, "does not support") ||
			strings.Contains(lower, "no such process") || strings.Contains(lower, "domain does not exist") {
			st.DomainFound = false
		}
		return st
	}

	depth := 0
	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasSuffix(trimmed, "{") {
			depth++
			continue
		}
		if trimmed == "}" {
			depth--
			continue
		}
		if depth != 1 {
			continue
		}
		key, value, ok := strings.Cut(trimmed, " = ")
		if !ok {
			continue
		}
		st.Raw[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}

	if len(st.Raw) == 0 {
		return st
	}
	st.Loaded = true
	st.State = st.Raw["state"]
	st.PID, _ = strconv.Atoi(st.Raw["pid"])
	st.Runs, _ = strconv.Atoi(st.Raw["runs"])
	st.LastExit = st.Raw["last exit code"]
	st.ExitTimeout, _ = strconv.Atoi(st.Raw["exit timeout"])
	st.Path = st.Raw["path"]
	st.Program = st.Raw["program"]
	st.StdoutPath = st.Raw["stdout path"]
	st.StderrPath = st.Raw["stderr path"]
	return st
}

// Running reports whether launchd shows a live process for the job.
func (st LaunchdStatus) Running() bool {
	return st.Loaded && st.PID > 0 && st.State == "running"
}

// Summary renders the status for humans, e.g.
// "running (pid 2933, runs 1, last exit: never exited)".
func (st LaunchdStatus) Summary() string {
	if !st.DomainFound {
		return "launchd domain not available"
	}
	if !st.Loaded {
		return "not loaded"
	}
	state := st.State
	if state == "" {
		state = "unknown"
	}
	parts := []string{}
	if st.PID > 0 {
		parts = append(parts, fmt.Sprintf("pid %d", st.PID))
	}
	if st.Runs > 0 {
		parts = append(parts, fmt.Sprintf("runs %d", st.Runs))
	}
	if st.LastExit != "" {
		parts = append(parts, "last exit: "+strings.Trim(st.LastExit, "()"))
	}
	if len(parts) == 0 {
		return state
	}
	return state + " (" + strings.Join(parts, ", ") + ")"
}

// launchdDomains lists where the job may live, in the order to probe: the
// intended domain first, then the domain the legacy `launchctl load -w`
// fallback used, then the system domain.
func launchdDomains(uid int) []string {
	return []string{fmt.Sprintf("gui/%d", uid), fmt.Sprintf("user/%d", uid), "system"}
}

// launchctlPrint runs `launchctl print <target>` and parses it. Reading a
// job's state works unprivileged in every domain, including system/; only
// mutations need sudo.
func (s Service) launchctlPrint(ctx context.Context, r Runner, target string) LaunchdStatus {
	stdout, stderr, _, _ := r.Run(ctx, "launchctl", "print", target)
	return ParseLaunchctlPrint(stdout, stderr)
}

// LaunchdStatus locates the job in whichever domain it is loaded in and
// returns its status and that domain. When the job is loaded nowhere the
// intended domain (s.Domain) is returned with Loaded == false.
func (s Service) LaunchdStatus(ctx context.Context, r Runner) (LaunchdStatus, string) {
	domains := []string{s.Domain}
	if s.Scope != ScopeSystem {
		for _, d := range launchdDomains(s.UID) {
			if d != s.Domain {
				domains = append(domains, d)
			}
		}
	}
	var first LaunchdStatus
	for i, d := range domains {
		st := s.launchctlPrint(ctx, r, d+"/"+s.Label)
		if i == 0 {
			first = st
		}
		if st.Loaded {
			return st, d
		}
	}
	return first, s.Domain
}

// GUIDomainAvailable reports whether launchd has a GUI login domain for uid
// — the domain a LaunchAgent is bootstrapped into. It is absent when nobody
// is logged in at the console (SSH-only access, the login window after a
// reboot). detail carries the "session = Aqua" line when present.
func GUIDomainAvailable(ctx context.Context, r Runner, uid int) (bool, string) {
	stdout, _, code, err := r.Run(ctx, "launchctl", "print", fmt.Sprintf("gui/%d", uid))
	if err != nil || code != 0 {
		return false, ""
	}
	for _, line := range strings.Split(stdout, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "session = ") {
			return true, t
		}
	}
	return true, ""
}
