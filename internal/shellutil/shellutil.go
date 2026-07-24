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

// Package shellutil resolves which shell the daemon should run commands in.
package shellutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/gelotto/hqsshd/internal/logging"
)

// UserShell returns the shell for sessions, tool detection, and tasks:
// $SHELL when set and present, otherwise the platform's default login shell.
// The platform default matters under service managers that start the daemon
// without $SHELL (launchd, systemd): on macOS that must be /bin/zsh — the
// default user shell since Catalina, and the only one whose login init
// (/etc/zprofile path_helper + ~/.zprofile) picks up Homebrew and
// version-manager PATH entries that tools like claude are installed under.
func UserShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		if _, err := os.Stat(shell); err == nil {
			return shell
		}
		logging.Warn("configured SHELL not found, using platform default",
			"shell", shell,
		)
	}

	fallback := "/bin/bash"
	if runtime.GOOS == "darwin" {
		fallback = "/bin/zsh"
	}
	if _, err := os.Stat(fallback); err == nil {
		return fallback
	}
	return "/bin/sh"
}

// EnsureUserPATH appends well-known user binary directories to the daemon's
// PATH when they exist but are missing from it. Service managers (launchd,
// systemd) start the daemon with a minimal PATH, and AI CLIs typically live
// in per-user directories that only interactive shell init adds — e.g.
// claude's native installer targets ~/.local/bin via ~/.zshrc, which
// non-interactive login shells never read. Fixing the daemon's own PATH
// repairs exec.LookPath tool detection directly, and login shells for
// sessions and tasks inherit it (both zsh's path_helper and typical
// ~/.profile PATH lines preserve pre-existing entries).
func EnsureUserPATH() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	candidates := []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "bin"),
		"/opt/homebrew/bin",
		"/usr/local/bin",
	}

	path := os.Getenv("PATH")
	have := make(map[string]bool)
	for _, dir := range strings.Split(path, string(os.PathListSeparator)) {
		have[dir] = true
	}

	var added []string
	for _, dir := range candidates {
		if have[dir] {
			continue
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		path += string(os.PathListSeparator) + dir
		added = append(added, dir)
	}
	if len(added) == 0 {
		return
	}
	os.Setenv("PATH", path)
	logging.Info("added user binary directories to PATH", "dirs", strings.Join(added, ":"))
}
