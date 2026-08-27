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

// Package servicemgr knows how hqsshd is installed as a background service
// (a launchd agent or daemon on macOS, a systemd user unit on Linux) and how
// to talk about it: which files define it, which domain it lives in, and the
// exact commands a user should run to start, stop, restart, inspect and read
// logs from it. The daemon uses it to render "already running — stop it
// with ..." hints; the CLI builds `hqssh service` and `hqssh doctor` on it.
//
// Service definitions themselves are written by scripts/install.sh; this
// package never installs anything.
package servicemgr

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const (
	// LaunchdLabel is the launchd job label (and plist basename) on macOS.
	LaunchdLabel = "com.gelotto.hqsshd"
	// SystemdUnit is the systemd user unit name on Linux.
	SystemdUnit = "hqsshd"
	// InstallURL serves the installer script.
	InstallURL = "https://hqssh.com/install"
	// InstallCommand is the one-line installer.
	InstallCommand = "curl -fsSL " + InstallURL + " | sh"
	// InstallSystemCommand installs the boot-time macOS system daemon.
	InstallSystemCommand = "curl -fsSL " + InstallURL + " | HQSSH_SERVICE_SCOPE=system sh"
)

// Kind is the service manager a service definition belongs to.
type Kind int

const (
	KindNone Kind = iota
	KindLaunchd
	KindSystemd
)

func (k Kind) String() string {
	switch k {
	case KindLaunchd:
		return "launchd"
	case KindSystemd:
		return "systemd"
	default:
		return "none"
	}
}

// Scope distinguishes a per-user service from a system-wide one.
type Scope int

const (
	// ScopeUser: launchd agent in ~/Library/LaunchAgents (starts at GUI
	// login) or systemd --user unit.
	ScopeUser Scope = iota
	// ScopeSystem: launchd daemon in /Library/LaunchDaemons running as the
	// user (starts at boot, needs sudo to manage).
	ScopeSystem
)

func (s Scope) String() string {
	if s == ScopeSystem {
		return "system"
	}
	return "user"
}

// Service describes one installed service definition.
type Service struct {
	Kind     Kind
	Scope    Scope
	Label    string // LaunchdLabel or SystemdUnit
	UnitPath string // plist or .service file
	Domain   string // launchd: "gui/<uid>", "user/<uid>" or "system"; systemd: ""
	UID      int
}

// goos is overridable in tests so both platforms' detection is exercised.
var goos = runtime.GOOS

// systemLaunchdPlist and the systemd unit locations are variables so tests
// can point them at a temp dir.
var (
	systemLaunchdPlist = "/Library/LaunchDaemons/" + LaunchdLabel + ".plist"
	systemSystemdUnit  = "/etc/systemd/user/" + SystemdUnit + ".service"
)

// UserLaunchdPlist returns the per-user agent plist path.
func UserLaunchdPlist() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", LaunchdLabel+".plist")
}

// UserSystemdUnit returns the per-user systemd unit path.
func UserSystemdUnit() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", SystemdUnit+".service")
}

// DetectAll returns every service definition found on disk, system scope
// first. An empty result means hqsshd is not installed as a service here.
func DetectAll() []Service {
	uid := os.Getuid()
	var found []Service
	switch goos {
	case "darwin":
		if fileExists(systemLaunchdPlist) {
			found = append(found, Service{Kind: KindLaunchd, Scope: ScopeSystem, Label: LaunchdLabel, UnitPath: systemLaunchdPlist, Domain: "system", UID: uid})
		}
		if p := UserLaunchdPlist(); fileExists(p) {
			found = append(found, Service{Kind: KindLaunchd, Scope: ScopeUser, Label: LaunchdLabel, UnitPath: p, Domain: fmt.Sprintf("gui/%d", uid), UID: uid})
		}
	case "linux":
		if p := UserSystemdUnit(); fileExists(p) {
			found = append(found, Service{Kind: KindSystemd, Scope: ScopeUser, Label: SystemdUnit, UnitPath: p, UID: uid})
		} else if fileExists(systemSystemdUnit) {
			found = append(found, Service{Kind: KindSystemd, Scope: ScopeUser, Label: SystemdUnit, UnitPath: systemSystemdUnit, UID: uid})
		}
	}
	return found
}

// Detect returns the preferred service definition (system over user) and
// whether one exists.
func Detect() (Service, bool) {
	all := DetectAll()
	if len(all) == 0 {
		return Service{}, false
	}
	return all[0], true
}

// Target is the launchd service specifier ("<domain>/<label>").
func (s Service) Target() string {
	return s.Domain + "/" + s.Label
}

// String renders a one-line description, e.g.
// "launchd gui/501/com.gelotto.hqsshd" or "systemd --user hqsshd".
func (s Service) String() string {
	switch s.Kind {
	case KindLaunchd:
		return "launchd " + s.Target()
	case KindSystemd:
		return "systemd --user " + s.Label
	default:
		return "no service"
	}
}

// NeedsSudo reports whether managing this service requires root.
func (s Service) NeedsSudo() bool {
	return s.Kind == KindLaunchd && s.Scope == ScopeSystem && os.Geteuid() != 0
}

func (s Service) sudo() string {
	if s.NeedsSudo() {
		return "sudo "
	}
	return ""
}

// StartCommand is the raw command that starts the service.
func (s Service) StartCommand() string {
	switch s.Kind {
	case KindLaunchd:
		return fmt.Sprintf("%slaunchctl bootstrap %s %s && %slaunchctl kickstart %s",
			s.sudo(), s.Domain, s.UnitPath, s.sudo(), s.Target())
	case KindSystemd:
		return "systemctl --user start " + s.Label
	default:
		return "hqsshd"
	}
}

// StopCommand is the raw command that stops the service. With
// KeepAlive.SuccessfulExit=false / Restart=on-failure a stopped service
// stays stopped until started again.
func (s Service) StopCommand() string {
	switch s.Kind {
	case KindLaunchd:
		return fmt.Sprintf("%slaunchctl bootout %s", s.sudo(), s.Target())
	case KindSystemd:
		return "systemctl --user stop " + s.Label
	default:
		return ""
	}
}

// RestartCommand is the raw command that restarts the service.
func (s Service) RestartCommand() string {
	switch s.Kind {
	case KindLaunchd:
		return fmt.Sprintf("%slaunchctl kickstart -k %s", s.sudo(), s.Target())
	case KindSystemd:
		return "systemctl --user restart " + s.Label
	default:
		return ""
	}
}

// StatusCommand is the raw command that shows the service manager's view.
func (s Service) StatusCommand() string {
	switch s.Kind {
	case KindLaunchd:
		return fmt.Sprintf("%slaunchctl print %s", s.sudo(), s.Target())
	case KindSystemd:
		return "systemctl --user status " + s.Label
	default:
		return ""
	}
}

// LogsCommand is the raw command that follows the daemon log.
func (s Service) LogsCommand() string {
	switch s.Kind {
	case KindLaunchd:
		return "tail -f " + DefaultLogFile()
	case KindSystemd:
		return "journalctl --user -u " + s.Label + " -f"
	default:
		return ""
	}
}

// StopHintFor renders the advice a second daemon prints when it finds a
// live one: the `hqssh service` form plus the raw service-manager command
// when a service is installed, otherwise a plain kill.
func StopHintFor(pid int) string {
	if svc, ok := Detect(); ok {
		return fmt.Sprintf("hqssh service stop   (or: %s)", svc.StopCommand())
	}
	if pid > 0 {
		return fmt.Sprintf("kill %d", pid)
	}
	return "hqssh doctor   (to find it)"
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
