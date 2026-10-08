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

package shellutil

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/gelotto/hqsshd/internal/logging"
)

// interactivePathMarker prefixes the PATH line so output that the user's
// shell init prints itself (banners, version-manager notices) is ignored.
const interactivePathMarker = "__HQSSH_PATH__="

// InteractivePATH returns $PATH as the user's interactive login shell sets
// it, the PATH sessions launch tools with. zsh -l never reads ~/.zshrc and
// a stock ~/.bashrc returns early when not interactive, which is where nvm,
// bun, volta and the claude installer add their PATH entries.
//
// The shell runs in its own session with stdin on /dev/null: no controlling
// tty, so init that prompts (oh-my-zsh updates) reads EOF instead of
// blocking, and a timeout kills everything it spawned. ok is false when the
// shell failed, timed out, or printed no PATH.
func InteractivePATH(timeout time.Duration) (path string, ok bool) {
	return InteractivePATHContext(context.Background(), timeout)
}

// InteractivePATHContext is InteractivePATH bounded by ctx as well: a
// cancelled ctx kills the probe shell and returns ok == false.
func InteractivePATHContext(parent context.Context, timeout time.Duration) (path string, ok bool) {
	shell := UserShell()

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, shell, "-l", "-i", "-c",
		`printf '\n%s%s\n' "$1" "$PATH"`, "_", interactivePathMarker)
	cmd.Stdout = &stdout
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 500 * time.Millisecond

	if err := cmd.Run(); err != nil {
		logging.Warn("interactive shell PATH probe failed",
			"shell", shell,
			"timeout", timeout,
			"error", err,
		)
		return "", false
	}
	return parseInteractivePATH(stdout.String())
}

// parseInteractivePATH returns the value of the last marker line.
func parseInteractivePATH(out string) (string, bool) {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if value, found := strings.CutPrefix(lines[i], interactivePathMarker); found {
			value = strings.TrimRight(value, "\r")
			return value, value != ""
		}
	}
	return "", false
}
