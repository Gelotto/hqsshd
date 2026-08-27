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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func withGOOS(t *testing.T, os_ string) {
	t.Helper()
	old := goos
	goos = os_
	t.Cleanup(func() { goos = old })
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDetectAll_Darwin(t *testing.T) {
	withGOOS(t, "darwin")
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldSys := systemLaunchdPlist
	systemLaunchdPlist = filepath.Join(home, "sys", LaunchdLabel+".plist")
	t.Cleanup(func() { systemLaunchdPlist = oldSys })

	if got := DetectAll(); len(got) != 0 {
		t.Fatalf("DetectAll() with nothing installed = %v", got)
	}
	if _, ok := Detect(); ok {
		t.Fatal("Detect() found a service with nothing installed")
	}

	touch(t, UserLaunchdPlist())
	got := DetectAll()
	if len(got) != 1 || got[0].Kind != KindLaunchd || got[0].Scope != ScopeUser {
		t.Fatalf("DetectAll() = %+v, want one user launchd agent", got)
	}
	if !strings.HasPrefix(got[0].Domain, "gui/") {
		t.Errorf("Domain = %q, want gui/<uid>", got[0].Domain)
	}
	if got[0].Target() != got[0].Domain+"/"+LaunchdLabel {
		t.Errorf("Target() = %q", got[0].Target())
	}

	touch(t, systemLaunchdPlist)
	got = DetectAll()
	if len(got) != 2 || got[0].Scope != ScopeSystem || got[0].Domain != "system" {
		t.Fatalf("DetectAll() = %+v, want system first", got)
	}
	svc, ok := Detect()
	if !ok || svc.Scope != ScopeSystem {
		t.Errorf("Detect() = %+v, %v; want the system daemon", svc, ok)
	}
}

func TestDetectAll_Linux(t *testing.T) {
	withGOOS(t, "linux")
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldSys := systemSystemdUnit
	systemSystemdUnit = filepath.Join(home, "etc", SystemdUnit+".service")
	t.Cleanup(func() { systemSystemdUnit = oldSys })

	if got := DetectAll(); len(got) != 0 {
		t.Fatalf("DetectAll() = %v, want none", got)
	}
	touch(t, systemSystemdUnit)
	got := DetectAll()
	if len(got) != 1 || got[0].Kind != KindSystemd || got[0].UnitPath != systemSystemdUnit {
		t.Fatalf("DetectAll() = %+v, want the /etc unit", got)
	}
	touch(t, UserSystemdUnit())
	got = DetectAll()
	if len(got) != 1 || got[0].UnitPath != UserSystemdUnit() {
		t.Fatalf("DetectAll() = %+v, want the user unit to win", got)
	}
}

func TestCommandRenderers(t *testing.T) {
	user := Service{Kind: KindLaunchd, Scope: ScopeUser, Label: LaunchdLabel,
		UnitPath: "/Users/g/Library/LaunchAgents/com.gelotto.hqsshd.plist", Domain: "gui/501", UID: 501}
	system := Service{Kind: KindLaunchd, Scope: ScopeSystem, Label: LaunchdLabel,
		UnitPath: "/Library/LaunchDaemons/com.gelotto.hqsshd.plist", Domain: "system", UID: 501}
	systemd := Service{Kind: KindSystemd, Scope: ScopeUser, Label: SystemdUnit}
	none := Service{}

	sudo := ""
	if os.Geteuid() != 0 {
		sudo = "sudo "
	}

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"user start", user.StartCommand(), "launchctl bootstrap gui/501 /Users/g/Library/LaunchAgents/com.gelotto.hqsshd.plist && launchctl kickstart gui/501/com.gelotto.hqsshd"},
		{"user stop", user.StopCommand(), "launchctl bootout gui/501/com.gelotto.hqsshd"},
		{"user restart", user.RestartCommand(), "launchctl kickstart -k gui/501/com.gelotto.hqsshd"},
		{"user status", user.StatusCommand(), "launchctl print gui/501/com.gelotto.hqsshd"},
		{"user string", user.String(), "launchd gui/501/com.gelotto.hqsshd"},
		{"system stop", system.StopCommand(), sudo + "launchctl bootout system/com.gelotto.hqsshd"},
		{"system restart", system.RestartCommand(), sudo + "launchctl kickstart -k system/com.gelotto.hqsshd"},
		{"systemd start", systemd.StartCommand(), "systemctl --user start hqsshd"},
		{"systemd stop", systemd.StopCommand(), "systemctl --user stop hqsshd"},
		{"systemd restart", systemd.RestartCommand(), "systemctl --user restart hqsshd"},
		{"systemd logs", systemd.LogsCommand(), "journalctl --user -u hqsshd -f"},
		{"systemd string", systemd.String(), "systemd --user hqsshd"},
		{"none start", none.StartCommand(), "hqsshd"},
		{"none string", none.String(), "no service"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s:\n got  %q\n want %q", c.name, c.got, c.want)
		}
	}
	if !strings.HasSuffix(user.LogsCommand(), "/.hqssh/logs/hqsshd.log") || !strings.HasPrefix(user.LogsCommand(), "tail -f ") {
		t.Errorf("user logs = %q", user.LogsCommand())
	}
	if system.NeedsSudo() != (os.Geteuid() != 0) || user.NeedsSudo() || systemd.NeedsSudo() {
		t.Error("NeedsSudo wrong")
	}
}

func TestStopHintFor(t *testing.T) {
	withGOOS(t, "darwin")
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldSys := systemLaunchdPlist
	systemLaunchdPlist = filepath.Join(home, "nope.plist")
	t.Cleanup(func() { systemLaunchdPlist = oldSys })

	if got := StopHintFor(1234); got != "kill 1234" {
		t.Errorf("no service, pid known: %q", got)
	}
	if got := StopHintFor(0); !strings.Contains(got, "hqssh doctor") {
		t.Errorf("no service, pid unknown: %q", got)
	}
	touch(t, UserLaunchdPlist())
	got := StopHintFor(1234)
	if !strings.HasPrefix(got, "hqssh service stop") || !strings.Contains(got, "launchctl bootout gui/") {
		t.Errorf("service installed: %q", got)
	}
}

func TestMaxSocketPathLen(t *testing.T) {
	want := 107
	if runtime.GOOS == "darwin" {
		want = 103
	}
	if got := MaxSocketPathLen(); got != want {
		t.Errorf("MaxSocketPathLen() = %d, want %d", got, want)
	}
}
