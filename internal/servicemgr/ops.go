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
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Operation tunables (variables for tests).
var (
	opTimeout = 15 * time.Second
	opPoll    = 250 * time.Millisecond
	// bootstrapRetries covers launchd's "Operation already in progress"
	// while a previous bootout is still tearing the job down.
	bootstrapRetries = 10
)

// StatusInfo is the service-manager-independent view of the service.
type StatusInfo struct {
	Service  Service
	Loaded   bool // launchd: job loaded; systemd: unit known
	Running  bool
	PID      int
	Summary  string // human-readable, e.g. "running (pid 2933, runs 1, ...)"
	Launchd  *LaunchdStatus
	Systemd  *SystemdStatus
	Domain   string // launchd: the domain the job was found in
	Problems []string
}

// Status queries the service manager.
func (s Service) Status(ctx context.Context, r Runner) (StatusInfo, error) {
	info := StatusInfo{Service: s, Domain: s.Domain}
	switch s.Kind {
	case KindLaunchd:
		st, domain := s.LaunchdStatus(ctx, r)
		info.Launchd = &st
		info.Domain = domain
		info.Loaded = st.Loaded
		info.Running = st.Running()
		info.PID = st.PID
		info.Summary = st.Summary()
		if !st.DomainFound {
			info.Problems = append(info.Problems, fmt.Sprintf("launchd domain %s is not available (no GUI login session)", s.Domain))
		}
		if domain != s.Domain && st.Loaded {
			info.Problems = append(info.Problems, fmt.Sprintf("job is loaded in %s, not %s (no GUI session when it was loaded): it stops with your last login session and does not start at boot; for a permanent daemon use %s", domain, s.Domain, InstallSystemCommand))
		}
		return info, nil
	case KindSystemd:
		st := s.SystemdStatus(ctx, r)
		info.Systemd = &st
		info.Loaded = st.Installed()
		info.Running = st.Running()
		info.PID = st.MainPID
		info.Summary = st.Summary()
		return info, nil
	default:
		return info, errors.New("no service installed")
	}
}

// LaunchctlError is a launchctl invocation that exited non-zero. Code is
// the exit status, which launchctl sets to its own error code (5 = already
// loaded / rejected plist, 37 = operation in progress, 125 = domain not
// available, 133 = service disabled).
type LaunchctlError struct {
	Cmd    string
	Code   int
	Stderr string
}

func (e *LaunchctlError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = "(no output)"
	}
	return fmt.Sprintf("%s failed (exit %d): %s", e.Cmd, e.Code, msg)
}

// launchctl runs a launchctl subcommand, with sudo when the service lives
// in the system domain.
func (s Service) launchctl(ctx context.Context, r Runner, args ...string) (string, error) {
	argv := append([]string{"launchctl"}, args...)
	if s.NeedsSudo() {
		argv = append([]string{"sudo"}, argv...)
	}
	stdout, stderr, code, err := r.Run(ctx, argv[0], argv[1:]...)
	if err != nil {
		return stdout, cmdError(argv[0], argv[1:], stderr, code, err)
	}
	if code != 0 {
		return stdout, &LaunchctlError{Cmd: strings.Join(argv, " "), Code: code, Stderr: stderr}
	}
	return stdout, nil
}

func (s Service) systemctl(ctx context.Context, r Runner, args ...string) error {
	argv := append([]string{"--user"}, args...)
	_, stderr, code, err := r.Run(ctx, "systemctl", argv...)
	if err != nil || code != 0 {
		return cmdError("systemctl", argv, stderr, code, err)
	}
	return nil
}

// Start loads the job if needed and makes sure a process is running.
func (s Service) Start(ctx context.Context, r Runner) error {
	switch s.Kind {
	case KindLaunchd:
		st, domain := s.LaunchdStatus(ctx, r)
		if !st.Loaded {
			loadedInto, err := s.bootstrap(ctx, r)
			if err != nil {
				return err
			}
			domain = loadedInto
			st, _ = s.LaunchdStatus(ctx, r) // RunAtLoad normally spawned it already
		}
		if st.PID == 0 {
			// A job that is loaded but exited cleanly needs a nudge
			if _, err := s.launchctl(ctx, r, "kickstart", domain+"/"+s.Label); err != nil {
				return err
			}
		}
		return s.waitLaunchd(ctx, r, func(st LaunchdStatus) bool { return st.PID > 0 }, "no process after start")
	case KindSystemd:
		if err := s.systemctl(ctx, r, "start", s.Label); err != nil {
			return err
		}
		return s.waitSystemd(ctx, r, func(st SystemdStatus) bool { return st.MainPID > 0 }, "no process after start")
	default:
		return errors.New("no service installed")
	}
}

// Stop unloads the job (launchd) or stops the unit (systemd) and waits for
// the process to be gone.
func (s Service) Stop(ctx context.Context, r Runner) error {
	switch s.Kind {
	case KindLaunchd:
		st, domain := s.LaunchdStatus(ctx, r)
		if !st.Loaded {
			return nil
		}
		if _, err := s.launchctl(ctx, r, "bootout", domain+"/"+s.Label); err != nil {
			return err
		}
		return s.waitLaunchd(ctx, r, func(st LaunchdStatus) bool { return !st.Loaded || st.PID == 0 }, "process still running after stop")
	case KindSystemd:
		if err := s.systemctl(ctx, r, "stop", s.Label); err != nil {
			return err
		}
		return s.waitSystemd(ctx, r, func(st SystemdStatus) bool { return st.MainPID == 0 }, "process still running after stop")
	default:
		return errors.New("no service installed")
	}
}

// Restart replaces the running process (kickstart -k / systemctl restart)
// and waits for a new pid.
func (s Service) Restart(ctx context.Context, r Runner) error {
	switch s.Kind {
	case KindLaunchd:
		st, domain := s.LaunchdStatus(ctx, r)
		if !st.Loaded {
			return s.Start(ctx, r)
		}
		old := st.PID
		if _, err := s.launchctl(ctx, r, "kickstart", "-k", domain+"/"+s.Label); err != nil {
			return err
		}
		return s.waitLaunchd(ctx, r, func(st LaunchdStatus) bool { return st.PID > 0 && st.PID != old }, "no new process after restart")
	case KindSystemd:
		st := s.SystemdStatus(ctx, r)
		old := st.MainPID
		if err := s.systemctl(ctx, r, "restart", s.Label); err != nil {
			return err
		}
		return s.waitSystemd(ctx, r, func(st SystemdStatus) bool { return st.MainPID > 0 && st.MainPID != old }, "no new process after restart")
	default:
		return errors.New("no service installed")
	}
}

// bootstrap loads the plist into the service's domain, retrying while a
// previous bootout is still in progress and clearing a stale "disabled"
// override first. A user agent whose GUI domain does not exist (SSH-only
// login) is loaded into user/<uid> instead — what the legacy `launchctl
// load -w` did — so an upgrade over SSH never leaves the daemon unloaded.
// Returns the domain the job was loaded into.
func (s Service) bootstrap(ctx context.Context, r Runner) (string, error) {
	domains := []string{s.Domain}
	if s.Scope == ScopeUser {
		domains = append(domains, fmt.Sprintf("user/%d", s.UID))
	}
	var lastErr error
	for _, domain := range domains {
		err := s.bootstrapInto(ctx, r, domain)
		if err == nil {
			return domain, nil
		}
		lastErr = err
		var le *LaunchctlError
		if !errors.As(err, &le) || launchctlCode(le) != 125 {
			return "", err
		}
		// 125: the domain is not available; try the next one
	}
	return "", fmt.Errorf("%w\n%s", lastErr, NoGUISessionHint())
}

func (s Service) bootstrapInto(ctx context.Context, r Runner, domain string) error {
	// A disabled override (launchctl disable, or an old `unload -w`) makes
	// bootstrap fail with "Service is disabled"; enable is harmless otherwise.
	s.launchctl(ctx, r, "enable", domain+"/"+s.Label) //nolint:errcheck

	var lastErr error
	for attempt := 0; attempt < bootstrapRetries; attempt++ {
		_, err := s.launchctl(ctx, r, "bootstrap", domain, s.UnitPath)
		if err == nil {
			return nil
		}
		lastErr = err
		var le *LaunchctlError
		if !errors.As(err, &le) {
			return err // could not run launchctl at all
		}
		switch launchctlCode(le) {
		case 37: // Operation already in progress: a previous bootout is still tearing the job down
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		case 125: // Domain does not support specified action / not found
			return err
		case 5: // Input/output error: already bootstrapped — or a plist launchd refuses
			if st := s.launchctlPrint(ctx, r, domain+"/"+s.Label); st.Loaded {
				return nil
			}
			return fmt.Errorf("%w\nlaunchd rejected the job: check `plutil -lint %s` and that the program it points to is executable", err, s.UnitPath)
		case 133: // Service is disabled
			return fmt.Errorf("%w\nthe service is disabled in launchd; run: launchctl enable %s/%s", err, domain, s.Label)
		default:
			return err
		}
	}
	return lastErr
}

// launchctlCode returns the launchd error code for a failed launchctl
// invocation: the exit status, or the "N:" in "Bootstrap failed: N: ..."
// when the exit status does not carry it.
func launchctlCode(e *LaunchctlError) int {
	if e.Code != 0 {
		return e.Code
	}
	m := launchctlCodeRe.FindStringSubmatch(e.Stderr)
	if m == nil {
		return 0
	}
	code, _ := strconv.Atoi(m[1])
	return code
}

var launchctlCodeRe = regexp.MustCompile(`(?:failed|error): (\d+):`)

// NoGUISessionHint explains the LaunchAgent login caveat and the fix.
func NoGUISessionHint() string {
	return "LaunchAgents live in the GUI login session, which does not exist until someone logs in at the Mac's console.\n" +
		"For a daemon that starts at boot and survives logout, install it as a system daemon:\n" +
		"  " + InstallSystemCommand + "\n" +
		"(Linux analog: loginctl enable-linger)"
}

// wait polls probe until it reports ok, giving up after opTimeout with the
// last summary in the error.
func (s Service) wait(ctx context.Context, probe func() (ok bool, summary string), failMsg string) error {
	deadline := time.Now().Add(opTimeout)
	for {
		ok, summary := probe()
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: %s reports %s", failMsg, s.Kind, summary)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opPoll):
		}
	}
}

func (s Service) waitLaunchd(ctx context.Context, r Runner, ok func(LaunchdStatus) bool, failMsg string) error {
	return s.wait(ctx, func() (bool, string) {
		st, _ := s.LaunchdStatus(ctx, r)
		return ok(st), st.Summary()
	}, failMsg)
}

func (s Service) waitSystemd(ctx context.Context, r Runner, ok func(SystemdStatus) bool, failMsg string) error {
	return s.wait(ctx, func() (bool, string) {
		st := s.SystemdStatus(ctx, r)
		return ok(st), st.Summary()
	}, failMsg)
}

// LogsCommandArgs returns the command that prints the daemon log: tail for
// launchd's log file, journalctl for systemd.
func (s Service) LogsCommandArgs(follow bool, lines int) (string, []string) {
	n := strconv.Itoa(lines)
	switch s.Kind {
	case KindSystemd:
		args := []string{"--user", "-u", s.Label, "-n", n, "--no-pager"}
		if follow {
			args = append(args, "-f")
		}
		return "journalctl", args
	default:
		args := []string{"-n", n}
		if follow {
			args = append(args, "-f")
		}
		return "tail", append(args, DefaultLogFile())
	}
}
