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
	"strings"
	"testing"
	"time"
)

func TestParseSystemctlShow(t *testing.T) {
	active := "LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nMainPID=4242\nNRestarts=0\n"
	st := ParseSystemctlShow(active, "")
	if !st.Installed() || !st.Running() || st.MainPID != 4242 {
		t.Errorf("active: %+v", st)
	}
	if st.Summary() != "active/running (pid 4242)" {
		t.Errorf("Summary() = %q", st.Summary())
	}

	failed := "LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=exit-code\nMainPID=0\nNRestarts=5\n"
	st = ParseSystemctlShow(failed, "")
	if !st.Installed() || st.Running() {
		t.Errorf("failed: %+v", st)
	}
	if st.Summary() != "failed/failed (restarts 5, result: exit-code)" {
		t.Errorf("Summary() = %q", st.Summary())
	}

	notFound := "LoadState=not-found\nActiveState=inactive\nSubState=dead\nResult=success\nMainPID=0\nNRestarts=0\n"
	st = ParseSystemctlShow(notFound, "")
	if st.Installed() {
		t.Error("not-found reported installed")
	}
	if st.Summary() != "not loaded" {
		t.Errorf("Summary() = %q", st.Summary())
	}

	st = ParseSystemctlShow("", "Failed to connect to bus: No medium found")
	if st.Installed() || !strings.Contains(st.Summary(), "Failed to connect to bus") {
		t.Errorf("bus failure: %+v", st)
	}
}

func TestParseLinger(t *testing.T) {
	if l, ok := ParseLinger("Linger=yes\n"); !l || !ok {
		t.Error("Linger=yes")
	}
	if l, ok := ParseLinger("Linger=no\n"); l || !ok {
		t.Error("Linger=no")
	}
	if _, ok := ParseLinger("garbage"); ok {
		t.Error("missing property reported ok")
	}
}

func TestSystemdOps(t *testing.T) {
	oldTimeout, oldPoll := opTimeout, opPoll
	opTimeout, opPoll = 2*time.Second, 10*time.Millisecond
	t.Cleanup(func() { opTimeout, opPoll = oldTimeout, oldPoll })

	svc := Service{Kind: KindSystemd, Scope: ScopeUser, Label: SystemdUnit}
	show := "systemctl --user show hqsshd -p " + systemdShowProps
	running := func(pid string) string {
		return "LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nMainPID=" + pid + "\nNRestarts=0\n"
	}

	restarted := false
	r := &FakeRunner{Handler: func(call string, n int) (FakeResponse, bool) {
		switch call {
		case show:
			if restarted {
				return FakeResponse{Stdout: running("77")}, true
			}
			return FakeResponse{Stdout: running("42")}, true
		case "systemctl --user restart hqsshd":
			restarted = true
			return FakeResponse{}, true
		case "systemctl --user stop hqsshd":
			return FakeResponse{Stderr: "Failed to stop hqsshd.service: Access denied", Code: 4}, true
		}
		return FakeResponse{}, false
	}}

	if err := svc.Restart(context.Background(), r); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	info, err := svc.Status(context.Background(), r)
	if err != nil || !info.Running || info.PID != 77 {
		t.Errorf("Status() = %+v, %v", info, err)
	}
	err = svc.Stop(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "Access denied") || !strings.Contains(err.Error(), "exit 4") {
		t.Errorf("Stop error = %v, want stderr surfaced", err)
	}
}
