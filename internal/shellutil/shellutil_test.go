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
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestUserShell_PrefersSHELL(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh") // Guaranteed to exist
	if got := UserShell(); got != "/bin/sh" {
		t.Errorf("UserShell() = %q, want /bin/sh", got)
	}
}

func TestUserShell_MissingSHELLFallsBackToPlatformDefault(t *testing.T) {
	t.Setenv("SHELL", "")
	got := UserShell()
	want := "/bin/bash"
	if runtime.GOOS == "darwin" {
		want = "/bin/zsh"
	}
	if _, err := os.Stat(want); err != nil {
		want = "/bin/sh"
	}
	if got != want {
		t.Errorf("UserShell() = %q, want %q", got, want)
	}
}

func TestUserShell_NonexistentSHELLFallsBack(t *testing.T) {
	t.Setenv("SHELL", "/nonexistent/shell")
	got := UserShell()
	if got == "/nonexistent/shell" {
		t.Error("UserShell() returned a shell that does not exist")
	}
}

func TestEnsureUserPATH(t *testing.T) {
	home := t.TempDir()
	binDir := home + "/.local/bin"
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")

	EnsureUserPATH()

	path := os.Getenv("PATH")
	if !strings.Contains(path, binDir) {
		t.Errorf("PATH %q missing existing user bin dir %q", path, binDir)
	}
	if strings.Contains(path, home+"/bin") {
		t.Errorf("PATH %q contains nonexistent dir %q", path, home+"/bin")
	}

	// Idempotent: a second call must not duplicate entries
	EnsureUserPATH()
	if got := strings.Count(os.Getenv("PATH"), binDir); got != 1 {
		t.Errorf("PATH contains %d copies of %q after second call, want 1", got, binDir)
	}
}
