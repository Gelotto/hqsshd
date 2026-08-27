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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Runner executes an external command and captures its output. The real
// implementation shells out; tests substitute a FakeRunner with canned
// launchctl/systemctl transcripts.
type Runner interface {
	// Run executes name with args. err is non-nil only when the command
	// could not be run at all (not found, context cancelled); a non-zero
	// exit is reported through exitCode with err == nil.
	Run(ctx context.Context, name string, args ...string) (stdout, stderr string, exitCode int, err error)
}

// ExecRunner runs commands with os/exec. stdin is inherited so `sudo` can
// prompt on the terminal.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Stdin = os.Stdin
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return stdout.String(), stderr.String(), exitErr.ExitCode(), nil
		}
		return stdout.String(), stderr.String(), -1, err
	}
	return stdout.String(), stderr.String(), 0, nil
}

// FakeResponse is one canned Runner result.
type FakeResponse struct {
	Stdout string
	Stderr string
	Code   int
	Err    error
}

// FakeRunner serves canned responses keyed by the full command line
// ("name arg1 arg2"). Handler, when set, is consulted first and can vary
// responses over successive calls (e.g. a pid appearing after kickstart).
type FakeRunner struct {
	Responses map[string]FakeResponse
	Handler   func(call string, n int) (FakeResponse, bool)
	Calls     []string
}

// Run implements Runner.
func (f *FakeRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	call := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.Calls = append(f.Calls, call)
	if f.Handler != nil {
		if r, ok := f.Handler(call, len(f.Calls)); ok {
			return r.Stdout, r.Stderr, r.Code, r.Err
		}
	}
	if r, ok := f.Responses[call]; ok {
		return r.Stdout, r.Stderr, r.Code, r.Err
	}
	return "", "fake runner: no response for " + call, 127, nil
}

// cmdError renders a failed command with its stderr, never swallowing it.
func cmdError(name string, args []string, stderr string, code int, err error) error {
	cmdline := strings.TrimSpace(name + " " + strings.Join(args, " "))
	if err != nil {
		return fmt.Errorf("%s: %w", cmdline, err)
	}
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = "(no output)"
	}
	return fmt.Errorf("%s failed (exit %d): %s", cmdline, code, msg)
}
