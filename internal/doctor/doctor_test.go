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

package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gelotto/hqsshd/internal/servicemgr"
)

func TestRender(t *testing.T) {
	r := Report{OS: "darwin", Arch: "arm64", CLIVersion: "v1.4.0", Checks: []Check{
		pass("binaries", "hqssh v1.4.0, hqsshd v1.4.0"),
		warn("path", "/Users/g/.local/bin is not in PATH", "export PATH=\"/Users/g/.local/bin:$PATH\""),
		fail("tcp", "127.0.0.1:50051: connection refused", "hqssh service restart\nsecond line"),
		skip("consistency", "needs both transports"),
	}, LogPath: "/Users/g/.hqssh/logs/hqsshd.log", LogTail: []string{"line one", "line two"}}

	var buf bytes.Buffer
	Render(&buf, r)
	out := buf.String()

	for _, want := range []string{
		"hqssh doctor — darwin/arm64, hqssh v1.4.0",
		"PASS  binaries",
		"WARN  path",
		"fix: export PATH=",
		"FAIL  tcp",
		"fix: hqssh service restart",
		"     second line",
		"SKIP  consistency",
		"--- last 2 lines of /Users/g/.hqssh/logs/hqsshd.log ---",
		"line one",
		"1 passed, 1 warning, 1 failed, 1 skipped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !r.Failed() {
		t.Error("Failed() = false with a FAIL check")
	}

	// No log tail block when nothing failed
	ok := Report{Checks: []Check{pass("a", "b")}, LogTail: []string{"x"}}
	buf.Reset()
	Render(&buf, ok)
	if strings.Contains(buf.String(), "--- last") {
		t.Error("log tail rendered without a failure")
	}
	if !strings.Contains(buf.String(), "1 passed, 0 warnings, 0 failed\n") {
		t.Errorf("summary = %q", buf.String())
	}
}

func TestRenderJSON(t *testing.T) {
	r := Report{OS: "linux", Arch: "amd64", CLIVersion: "v1.4.0",
		Checks: []Check{fail("socket", "no socket", "hqssh service start")}}
	var buf bytes.Buffer
	if err := RenderJSON(&buf, r); err != nil {
		t.Fatal(err)
	}
	var back struct {
		OS     string `json:"os"`
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Hint   string `json:"hint"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if back.OS != "linux" || len(back.Checks) != 1 || back.Checks[0].Status != "FAIL" || back.Checks[0].Hint != "hqssh service start" {
		t.Errorf("roundtrip = %+v", back)
	}
}

// TestRunWithoutDaemon exercises the whole pipeline against an empty HOME
// and a socket path nobody listens on: it must complete, fail the socket
// and tcp checks with actionable hints, and never hang.
func TestRunWithoutDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	sock := filepath.Join(home, "nope.sock")

	// A FakeRunner so no launchctl/systemctl/codesign is actually invoked
	r := &servicemgr.FakeRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	report := Run(ctx, Options{SocketPath: sock, TCPAddr: "127.0.0.1:1", Runner: r, Timeout: time.Second})

	if !report.Failed() {
		t.Fatal("report should fail with no daemon")
	}
	byName := map[string]Check{}
	for _, c := range report.Checks {
		byName[c.Name] = c
	}
	if c := byName["socket"]; c.Status != Fail || !strings.Contains(c.Detail, "no socket at "+sock) {
		t.Errorf("socket check = %+v", c)
	}
	if c := byName["tcp"]; c.Status != Fail || c.Hint == "" {
		t.Errorf("tcp check = %+v", c)
	}
	if c := byName["service"]; c.Status != Warn || !strings.Contains(c.Hint, "hqssh.com/install") {
		t.Errorf("service check = %+v", c)
	}
	if c := byName["consistency"]; c.Status != Skip {
		t.Errorf("consistency check = %+v", c)
	}
	if c := byName["config"]; c.Status != Pass || !strings.Contains(c.Detail, "defaults") {
		t.Errorf("config check = %+v", c)
	}
	// Data dir does not exist yet in the fresh HOME
	if c := byName["data-dir"]; c.Status != Warn {
		t.Errorf("data-dir check = %+v", c)
	}
}

func TestTailLinesAndCountErrors(t *testing.T) {
	if got := tailLines("", 5); got != nil {
		t.Errorf("tailLines(empty) = %v", got)
	}
	got := tailLines("a\nb\nc\nd\n", 2)
	if strings.Join(got, ",") != "c,d" {
		t.Errorf("tailLines = %v", got)
	}
	if s := countErrors([]string{"level=INFO x", "level=ERROR y", `{"level":"ERROR"}`}); s != "2 ERROR lines in the last 3" {
		t.Errorf("countErrors = %q", s)
	}
	if s := countErrors([]string{"level=INFO x"}); !strings.HasPrefix(s, "no errors") {
		t.Errorf("countErrors = %q", s)
	}
}

func TestCheckDataDirFlagsTempFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".hqssh")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "projects.json.tmp"), nil, 0600)

	checks := checkDataDir(&env{opts: Options{}, ctx: context.Background()})
	if len(checks) != 1 || checks[0].Status != Warn || !strings.Contains(checks[0].Detail, "projects.json.tmp") {
		t.Errorf("checkDataDir = %+v", checks)
	}
}
