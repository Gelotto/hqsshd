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

package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionWorkingDir_LocalResolvesAgainstCwd(t *testing.T) {
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	if err := os.Mkdir("sub", 0o755); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"":        cwd,
		".":       cwd,
		"sub":     filepath.Join(cwd, "sub"),
		"./sub/":  filepath.Join(cwd, "sub"),
		"/abs/ok": "/abs/ok",
	}
	for in, want := range cases {
		got, err := sessionWorkingDir(in, true)
		if err != nil {
			t.Fatalf("sessionWorkingDir(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("sessionWorkingDir(%q, local) = %q, want %q", in, got, want)
		}
	}
}

// The local cwd means nothing on a remote host: pass the value through and
// let the daemon resolve it against the remote home.
func TestSessionWorkingDir_RemotePassesThrough(t *testing.T) {
	for _, in := range []string{"", ".", "src/a", "/abs"} {
		got, err := sessionWorkingDir(in, false)
		if err != nil || got != in {
			t.Errorf("sessionWorkingDir(%q, remote) = %q, %v; want %q", in, got, err, in)
		}
	}
}
