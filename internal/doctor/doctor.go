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

// Package doctor diagnoses a local hqsshd installation: binaries, the
// background service (launchd or systemd), the socket and TCP transports,
// configuration, logs and data. It runs without a daemon and every failed
// check carries the command that fixes it.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/gelotto/hqsshd/internal/cli/client"
	"github.com/gelotto/hqsshd/internal/config"
	"github.com/gelotto/hqsshd/internal/servicemgr"
)

// Status is a check's verdict.
type Status int

const (
	Pass Status = iota
	Warn
	Fail
	Skip
)

func (s Status) String() string {
	switch s {
	case Pass:
		return "PASS"
	case Warn:
		return "WARN"
	case Fail:
		return "FAIL"
	default:
		return "SKIP"
	}
}

// MarshalText renders the status as its name in JSON.
func (s Status) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Check is one diagnostic result.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// Report is the outcome of a doctor run.
type Report struct {
	OS         string   `json:"os"`
	Arch       string   `json:"arch"`
	CLIVersion string   `json:"cli_version"`
	Checks     []Check  `json:"checks"`
	LogPath    string   `json:"log_path,omitempty"`
	LogTail    []string `json:"log_tail,omitempty"`
}

// Failed reports whether any check failed.
func (r Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

// Counts returns the number of passed, warning, failed and skipped checks.
func (r Report) Counts() (pass, warn, fail, skip int) {
	for _, c := range r.Checks {
		switch c.Status {
		case Pass:
			pass++
		case Warn:
			warn++
		case Fail:
			fail++
		default:
			skip++
		}
	}
	return
}

// Options configure a run. Zero values pick the daemon defaults.
type Options struct {
	SocketPath string
	TCPAddr    string
	ConfigPath string // "" = ~/.hqssh/daemon.yaml
	Runner     servicemgr.Runner
	Timeout    time.Duration // per external probe
	LogLines   int           // lines of log tail to capture on failure
}

// defaults fills unset options from the daemon's own configuration, so a
// custom `socket:` or `tcp_port:` in daemon.yaml is probed rather than the
// compile-time defaults. cfg may be nil (unreadable config).
func (o *Options) defaults(cfg *config.Config) {
	if o.SocketPath == "" {
		if cfg != nil && cfg.Socket != "" {
			o.SocketPath = cfg.Socket
		} else {
			o.SocketPath = client.DefaultSocketPath
		}
	}
	if o.TCPAddr == "" {
		switch {
		case cfg != nil && cfg.TCPPort > 0:
			o.TCPAddr = fmt.Sprintf("127.0.0.1:%d", cfg.TCPPort)
		case cfg != nil:
			o.TCPAddr = "" // disabled; checkTCP explains
		default:
			o.TCPAddr = client.DefaultTCPAddr
		}
	}
	if o.Runner == nil {
		o.Runner = servicemgr.ExecRunner{}
	}
	if o.Timeout == 0 {
		o.Timeout = 3 * time.Second
	}
	if o.LogLines == 0 {
		o.LogLines = 20
	}
}

// Run executes every check and returns the report.
func Run(ctx context.Context, o Options) Report {
	e := &env{opts: o, ctx: ctx}
	cfg, _ := e.loadConfig()
	o.defaults(cfg)
	e.opts = o
	if cfg != nil {
		e.token = cfg.AuthToken
	}
	r := Report{OS: runtime.GOOS, Arch: runtime.GOARCH, CLIVersion: config.DaemonVersion}

	for _, step := range []func(*env) []Check{
		checkBinaries,
		checkPath,
		checkService,
		checkPidfile,
		checkSocket,
		checkTCP,
		checkBootDelay,
		checkConsistency,
		checkConfig,
		checkLog,
		checkDataDir,
		checkLsof,
		checkCodesign,
		checkTools,
	} {
		r.Checks = append(r.Checks, step(e)...)
	}

	r.LogPath = e.logPath
	if r.Failed() {
		r.LogTail = e.logTail
	}
	return r
}

// Render prints the report as aligned text. The log tail is shown only
// when something failed.
func Render(w io.Writer, r Report) {
	fmt.Fprintf(w, "hqssh doctor — %s/%s, hqssh %s\n\n", r.OS, r.Arch, r.CLIVersion)

	nameWidth := 0
	for _, c := range r.Checks {
		if len(c.Name) > nameWidth {
			nameWidth = len(c.Name)
		}
	}
	for _, c := range r.Checks {
		fmt.Fprintf(w, "%-4s  %-*s  %s\n", c.Status, nameWidth, c.Name, c.Detail)
		if c.Hint != "" && c.Status != Pass {
			for i, line := range strings.Split(c.Hint, "\n") {
				prefix := "fix: "
				if i > 0 {
					prefix = "     "
				}
				fmt.Fprintf(w, "%-4s  %-*s  %s%s\n", "", nameWidth, "", prefix, line)
			}
		}
	}

	if r.Failed() && len(r.LogTail) > 0 {
		fmt.Fprintf(w, "\n--- last %d lines of %s ---\n", len(r.LogTail), r.LogPath)
		for _, line := range r.LogTail {
			fmt.Fprintln(w, line)
		}
	}

	pass, warn, fail, skip := r.Counts()
	fmt.Fprintf(w, "\n%d passed, %d warning%s, %d failed", pass, warn, plural(warn), fail)
	if skip > 0 {
		fmt.Fprintf(w, ", %d skipped", skip)
	}
	fmt.Fprintln(w)
}

// RenderJSON prints the report as JSON.
func RenderJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
