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
	"strconv"
	"strings"
	"testing"
	"time"
)

// launchctlPrintRunning is a verbatim (trimmed) capture from macOS 26.5.
// Note the nested "state = active" inside a sub-block, which must not
// override the top-level "state = running".
const launchctlPrintRunning = `gui/501/com.gelotto.hqsshd = {
	active count = 1
	path = /Users/gelotto/Library/LaunchAgents/com.gelotto.hqsshd.plist
	type = LaunchAgent
	state = running

	program = /Users/gelotto/.local/bin/hqsshd
	arguments = {
		/Users/gelotto/.local/bin/hqsshd
	}

	working directory = /Users/gelotto
	stdout path = /Users/gelotto/.hqssh/logs/hqsshd.log
	stderr path = /Users/gelotto/.hqssh/logs/hqsshd.log
	default environment = {
		PATH => /usr/bin:/bin:/usr/sbin:/sbin
	}

	environment = {
		HOME => /Users/gelotto
		XPC_SERVICE_NAME => com.gelotto.hqsshd
	}

	domain = gui/501 [100002]
	minimum runtime = 10
	exit timeout = 5
	runs = 1
	pid = 2933
	immediate reason = speculative
	forks = 0
	execs = 1
	initialized = 1
	trampolined = 1
	started suspended = 0
	proxy started suspended = 0
	last exit code = (never exited)

	semaphores = {
		successful exit => 0
	}

	event triggers = {
	}

	endpoints = {
	}

	dynamic endpoints = {
	}

	pid-local endpoints = {
	}

	instance-specific endpoints = {
	}

	environment endpoints = {
	}

	resource coalition = {
		state = active
		id = 12345
	}

	spawn type = daemon (3)
	spawn role = background (2)
	jetsam priority = 3
	jetsam memory limit (active) = (unlimited)
	jetsam memory limit (inactive) = (unlimited)
	jetsamproperties category = daemon
	submitted job. ignore execute allowed
	cs blob
	properties = runatload | inferred program | managed LWCR | has LWCR
}
`

const launchctlPrintExited = `gui/501/com.gelotto.hqsshd = {
	active count = 0
	path = /Users/gelotto/Library/LaunchAgents/com.gelotto.hqsshd.plist
	state = not running

	program = /Users/gelotto/.local/bin/hqsshd
	exit timeout = 30
	runs = 3
	last exit code = 4

	semaphores = {
		successful exit => 0
	}
}
`

func TestParseLaunchctlPrint_Running(t *testing.T) {
	st := ParseLaunchctlPrint(launchctlPrintRunning, "")
	if !st.Loaded || !st.DomainFound {
		t.Fatalf("Loaded=%v DomainFound=%v", st.Loaded, st.DomainFound)
	}
	if st.State != "running" {
		t.Errorf("State = %q (nested 'state = active' must not win)", st.State)
	}
	if st.PID != 2933 || st.Runs != 1 || st.ExitTimeout != 5 {
		t.Errorf("PID=%d Runs=%d ExitTimeout=%d", st.PID, st.Runs, st.ExitTimeout)
	}
	if st.LastExit != "(never exited)" {
		t.Errorf("LastExit = %q", st.LastExit)
	}
	if st.Program != "/Users/gelotto/.local/bin/hqsshd" || !strings.HasSuffix(st.StdoutPath, "hqsshd.log") {
		t.Errorf("Program=%q StdoutPath=%q", st.Program, st.StdoutPath)
	}
	if !st.Running() {
		t.Error("Running() = false")
	}
	if got := st.Summary(); got != "running (pid 2933, runs 1, last exit: never exited)" {
		t.Errorf("Summary() = %q", got)
	}
	if _, nested := st.Raw["id"]; nested {
		t.Error("nested block key leaked into Raw")
	}
}

func TestParseLaunchctlPrint_Exited(t *testing.T) {
	st := ParseLaunchctlPrint(launchctlPrintExited, "")
	if !st.Loaded || st.Running() {
		t.Errorf("Loaded=%v Running=%v", st.Loaded, st.Running())
	}
	if st.State != "not running" || st.PID != 0 || st.Runs != 3 || st.LastExit != "4" || st.ExitTimeout != 30 {
		t.Errorf("parsed %+v", st)
	}
	if got := st.Summary(); got != "not running (runs 3, last exit: 4)" {
		t.Errorf("Summary() = %q", got)
	}
}

func TestParseLaunchctlPrint_NotLoadedAndNoDomain(t *testing.T) {
	st := ParseLaunchctlPrint("", `Could not find service "com.gelotto.hqsshd" in domain for user gui: 501`)
	if st.Loaded || !st.DomainFound {
		t.Errorf("not-found: Loaded=%v DomainFound=%v", st.Loaded, st.DomainFound)
	}
	if st.Summary() != "not loaded" {
		t.Errorf("Summary() = %q", st.Summary())
	}

	st = ParseLaunchctlPrint("", "Could not find domain for gui/501")
	if st.Loaded || st.DomainFound {
		t.Errorf("no-domain: Loaded=%v DomainFound=%v", st.Loaded, st.DomainFound)
	}
	if st.Summary() != "launchd domain not available" {
		t.Errorf("Summary() = %q", st.Summary())
	}
}

func TestLaunchdStatus_FindsJobInLegacyDomain(t *testing.T) {
	svc := Service{Kind: KindLaunchd, Scope: ScopeUser, Label: LaunchdLabel, Domain: "gui/501", UID: 501,
		UnitPath: "/Users/g/Library/LaunchAgents/com.gelotto.hqsshd.plist"}
	r := &FakeRunner{Responses: map[string]FakeResponse{
		"launchctl print gui/501/com.gelotto.hqsshd":  {Stderr: `Could not find service "com.gelotto.hqsshd" in domain for user gui: 501`, Code: 113},
		"launchctl print user/501/com.gelotto.hqsshd": {Stdout: launchctlPrintRunning},
	}}
	st, domain := svc.LaunchdStatus(context.Background(), r)
	if !st.Loaded || domain != "user/501" {
		t.Errorf("Loaded=%v domain=%q", st.Loaded, domain)
	}
	info, err := svc.Status(context.Background(), r)
	if err != nil || !info.Running || info.PID != 2933 || info.Domain != "user/501" {
		t.Errorf("Status() = %+v, %v", info, err)
	}
	if len(info.Problems) != 1 || !strings.Contains(info.Problems[0], "user/501") {
		t.Errorf("Problems = %v, want the legacy-domain warning", info.Problems)
	}
}

func TestGUIDomainAvailable(t *testing.T) {
	r := &FakeRunner{Responses: map[string]FakeResponse{
		"launchctl print gui/501": {Stdout: "gui/501 = {\n\ttype = login\n\thandle = 100002\n\tsession = Aqua\n}\n"},
		"launchctl print gui/502": {Stderr: "Could not find domain for gui/502", Code: 125},
	}}
	ok, detail := GUIDomainAvailable(context.Background(), r, 501)
	if !ok || detail != "session = Aqua" {
		t.Errorf("gui/501: ok=%v detail=%q", ok, detail)
	}
	if ok, _ := GUIDomainAvailable(context.Background(), r, 502); ok {
		t.Error("gui/502 should be unavailable")
	}
}

func TestLaunchdOps(t *testing.T) {
	oldTimeout, oldPoll := opTimeout, opPoll
	opTimeout, opPoll = 2*time.Second, 10*time.Millisecond
	t.Cleanup(func() { opTimeout, opPoll = oldTimeout, oldPoll })

	svc := Service{Kind: KindLaunchd, Scope: ScopeUser, Label: LaunchdLabel, Domain: "gui/501", UID: 501,
		UnitPath: "/Users/g/Library/LaunchAgents/com.gelotto.hqsshd.plist"}
	printRunningWith := func(pid int) string {
		return strings.Replace(launchctlPrintRunning, "pid = 2933", "pid = "+strconv.Itoa(pid), 1)
	}

	t.Run("restart waits for a new pid", func(t *testing.T) {
		kicked := false
		r := &FakeRunner{Handler: func(call string, n int) (FakeResponse, bool) {
			switch call {
			case "launchctl print gui/501/com.gelotto.hqsshd":
				if kicked {
					return FakeResponse{Stdout: printRunningWith(4000)}, true
				}
				return FakeResponse{Stdout: printRunningWith(2933)}, true
			case "launchctl kickstart -k gui/501/com.gelotto.hqsshd":
				kicked = true
				return FakeResponse{}, true
			}
			return FakeResponse{}, false
		}}
		if err := svc.Restart(context.Background(), r); err != nil {
			t.Fatalf("Restart: %v", err)
		}
		if !kicked {
			t.Error("kickstart -k was not called")
		}
	})

	t.Run("start bootstraps when not loaded and reports launchctl stderr", func(t *testing.T) {
		loaded := false
		r := &FakeRunner{Handler: func(call string, n int) (FakeResponse, bool) {
			switch {
			case strings.HasPrefix(call, "launchctl print "):
				if loaded {
					return FakeResponse{Stdout: printRunningWith(5000)}, true
				}
				return FakeResponse{Stderr: `Could not find service "com.gelotto.hqsshd" in domain for user gui: 501`, Code: 113}, true
			case call == "launchctl enable gui/501/com.gelotto.hqsshd":
				return FakeResponse{}, true
			case call == "launchctl bootstrap gui/501 "+svc.UnitPath:
				loaded = true
				return FakeResponse{}, true
			}
			return FakeResponse{}, false
		}}
		if err := svc.Start(context.Background(), r); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !loaded {
			t.Error("bootstrap was not called")
		}
	})

	t.Run("bootstrap failure surfaces stderr and the GUI hint", func(t *testing.T) {
		var bootstraps []string
		r := &FakeRunner{Handler: func(call string, n int) (FakeResponse, bool) {
			switch {
			case strings.HasPrefix(call, "launchctl print "):
				return FakeResponse{Stderr: "Could not find domain for gui/501", Code: 125}, true
			case strings.HasPrefix(call, "launchctl enable "):
				return FakeResponse{}, true
			case strings.HasPrefix(call, "launchctl bootstrap "):
				bootstraps = append(bootstraps, call)
				return FakeResponse{Stderr: "Bootstrap failed: 125: Domain does not support specified action", Code: 125}, true
			}
			return FakeResponse{}, false
		}}
		err := svc.Start(context.Background(), r)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "Domain does not support specified action") {
			t.Errorf("stderr not surfaced: %v", err)
		}
		if !strings.Contains(err.Error(), "Log in at the Mac once") {
			t.Errorf("GUI hint missing: %v", err)
		}
		// gui/501 first, then the user/501 fallback the legacy loader used
		if len(bootstraps) != 2 || !strings.Contains(bootstraps[1], "user/501") {
			t.Errorf("bootstrap attempts = %v, want gui then user domain", bootstraps)
		}
	})

	t.Run("no GUI domain falls back to user/<uid>", func(t *testing.T) {
		loadedUser := false
		r := &FakeRunner{Handler: func(call string, n int) (FakeResponse, bool) {
			switch {
			case call == "launchctl print gui/501/com.gelotto.hqsshd":
				return FakeResponse{Stderr: "Could not find domain for gui/501", Code: 125}, true
			case call == "launchctl print user/501/com.gelotto.hqsshd":
				if loadedUser {
					return FakeResponse{Stdout: strings.Replace(launchctlPrintRunning, "gui/501", "user/501", 1)}, true
				}
				return FakeResponse{Stderr: "Could not find service", Code: 113}, true
			case call == "launchctl print system/com.gelotto.hqsshd":
				return FakeResponse{Stderr: "Could not find service", Code: 113}, true
			case strings.HasPrefix(call, "launchctl enable "):
				return FakeResponse{}, true
			case call == "launchctl bootstrap gui/501 "+svc.UnitPath:
				return FakeResponse{Stderr: "Bootstrap failed: 125: Domain does not support specified action", Code: 125}, true
			case call == "launchctl bootstrap user/501 "+svc.UnitPath:
				loadedUser = true
				return FakeResponse{}, true
			}
			return FakeResponse{}, false
		}}
		if err := svc.Start(context.Background(), r); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !loadedUser {
			t.Error("user/501 bootstrap was not attempted")
		}
		info, _ := svc.Status(context.Background(), r)
		if info.Domain != "user/501" || len(info.Problems) != 1 {
			t.Errorf("Status = domain %q problems %v", info.Domain, info.Problems)
		}
	})

	t.Run("stop boots out and waits", func(t *testing.T) {
		booted := false
		r := &FakeRunner{Handler: func(call string, n int) (FakeResponse, bool) {
			switch call {
			case "launchctl print gui/501/com.gelotto.hqsshd":
				if booted {
					return FakeResponse{Stderr: "Could not find service", Code: 113}, true
				}
				return FakeResponse{Stdout: launchctlPrintRunning}, true
			case "launchctl bootout gui/501/com.gelotto.hqsshd":
				booted = true
				return FakeResponse{}, true
			}
			return FakeResponse{}, false
		}}
		if err := svc.Stop(context.Background(), r); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if !booted {
			t.Error("bootout was not called")
		}
	})
}

func TestLogsCommandArgs(t *testing.T) {
	launchd := Service{Kind: KindLaunchd, Label: LaunchdLabel}
	name, args := launchd.LogsCommandArgs(true, 50)
	if name != "tail" || strings.Join(args, " ") != "-n 50 -f "+DefaultLogFile() {
		t.Errorf("launchd logs = %s %v", name, args)
	}
	systemd := Service{Kind: KindSystemd, Label: SystemdUnit}
	name, args = systemd.LogsCommandArgs(false, 20)
	if name != "journalctl" || strings.Join(args, " ") != "--user -u hqsshd -n 20 --no-pager" {
		t.Errorf("systemd logs = %s %v", name, args)
	}
}
