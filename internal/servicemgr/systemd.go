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

// SystemdStatus is systemd's view of the user unit, parsed from
// `systemctl --user show hqsshd -p ...`.
type SystemdStatus struct {
	LoadState   string // loaded, not-found
	ActiveState string // active, inactive, failed, activating
	SubState    string // running, dead, auto-restart
	Result      string // success, exit-code, signal, ...
	MainPID     int
	NRestarts   int
	Raw         map[string]string
	Stderr      string
}

// systemdShowProps are the properties Status asks systemctl for.
var systemdShowProps = "LoadState,ActiveState,SubState,Result,MainPID,NRestarts"

// ParseSystemctlShow parses KEY=VALUE lines from `systemctl show`.
func ParseSystemctlShow(stdout, stderr string) SystemdStatus {
	st := SystemdStatus{Raw: map[string]string{}, Stderr: strings.TrimSpace(stderr)}
	for _, line := range strings.Split(stdout, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key == "" {
			continue
		}
		st.Raw[key] = value
	}
	st.LoadState = st.Raw["LoadState"]
	st.ActiveState = st.Raw["ActiveState"]
	st.SubState = st.Raw["SubState"]
	st.Result = st.Raw["Result"]
	st.MainPID, _ = strconv.Atoi(st.Raw["MainPID"])
	st.NRestarts, _ = strconv.Atoi(st.Raw["NRestarts"])
	return st
}

// Installed reports whether systemd knows the unit.
func (st SystemdStatus) Installed() bool {
	return st.LoadState != "" && st.LoadState != "not-found"
}

// Running reports whether the unit's main process is up.
func (st SystemdStatus) Running() bool {
	return st.ActiveState == "active" && st.MainPID > 0
}

// Summary renders the status for humans, e.g. "active/running (pid 4242,
// restarts 0)".
func (st SystemdStatus) Summary() string {
	if !st.Installed() {
		if st.Stderr != "" {
			return "unavailable: " + st.Stderr
		}
		return "not loaded"
	}
	s := st.ActiveState
	if st.SubState != "" {
		s += "/" + st.SubState
	}
	parts := []string{}
	if st.MainPID > 0 {
		parts = append(parts, fmt.Sprintf("pid %d", st.MainPID))
	}
	if st.NRestarts > 0 {
		parts = append(parts, fmt.Sprintf("restarts %d", st.NRestarts))
	}
	if st.Result != "" && st.Result != "success" {
		parts = append(parts, "result: "+st.Result)
	}
	if len(parts) == 0 {
		return s
	}
	return s + " (" + strings.Join(parts, ", ") + ")"
}

// ParseLinger parses `loginctl show-user <user> -p Linger`. ok is false
// when the property is absent (loginctl failed).
func ParseLinger(stdout string) (linger bool, ok bool) {
	for _, line := range strings.Split(stdout, "\n") {
		if v, found := strings.CutPrefix(strings.TrimSpace(line), "Linger="); found {
			return v == "yes", true
		}
	}
	return false, false
}

// SystemdStatus queries systemctl for the unit.
func (s Service) SystemdStatus(ctx context.Context, r Runner) SystemdStatus {
	stdout, stderr, _, err := r.Run(ctx, "systemctl", "--user", "show", s.Label, "-p", systemdShowProps)
	st := ParseSystemctlShow(stdout, stderr)
	if err != nil && st.Stderr == "" {
		st.Stderr = err.Error()
	}
	return st
}

// LingerEnabled reports whether the user's session lingers (services keep
// running after logout and start at boot).
func LingerEnabled(ctx context.Context, r Runner, user string) (linger bool, ok bool) {
	stdout, _, code, err := r.Run(ctx, "loginctl", "show-user", user, "-p", "Linger")
	if err != nil || code != 0 {
		return false, false
	}
	return ParseLinger(stdout)
}
