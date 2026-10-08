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

package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gelotto/hqsshd/internal/config"
)

func newTestDetector(t *testing.T, tools ...config.ToolConfig) *Detector {
	t.Helper()
	t.Setenv("SHELL", "/bin/sh") // fast, no profile
	cfg := config.DefaultConfig()
	cfg.Tools = tools
	return NewDetector(cfg)
}

func TestDetectAll_TimeoutBoundsSlowLookup(t *testing.T) {
	d := newTestDetector(t, config.ToolConfig{
		Name: "hqssh-slow-tool", Command: "hqssh-slow-tool", Detect: "sleep 30",
	})
	d.timeout = 200 * time.Millisecond

	start := time.Now()
	installed := d.DetectAll()
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("DetectAll took %v, timeout not enforced", elapsed)
	}
	if len(installed) != 0 {
		t.Errorf("timed-out tool reported installed: %v", installed)
	}
}

func TestDetectAll_ProbesToolsConcurrently(t *testing.T) {
	d := newTestDetector(t,
		config.ToolConfig{Name: "hqssh-t1", Command: "hqssh-t1", Detect: "sleep 1"},
		config.ToolConfig{Name: "hqssh-t2", Command: "hqssh-t2", Detect: "sleep 1"},
	)
	start := time.Now()
	installed := d.DetectAll()
	elapsed := time.Since(start)

	if elapsed > 1900*time.Millisecond {
		t.Errorf("DetectAll took %v; two 1s probes should overlap", elapsed)
	}
	if len(installed) != 2 {
		t.Errorf("installed = %v, want both", installed)
	}
}

func TestDetectAll_CachesWithinTTL(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "calls")
	d := newTestDetector(t, config.ToolConfig{
		Name: "hqssh-count", Command: "hqssh-count", Detect: "echo x >> " + marker,
	})

	if got := d.DetectAll(); len(got) != 1 || got[0] != "hqssh-count" {
		t.Fatalf("first DetectAll = %v", got)
	}
	d.DetectAll()
	if n := countLines(t, marker); n != 1 {
		t.Errorf("probe ran %d times within TTL, want 1", n)
	}

	d.ClearCache()
	d.DetectAll()
	if n := countLines(t, marker); n != 2 {
		t.Errorf("probe ran %d times after ClearCache, want 2", n)
	}

	// An expired TTL serves the stale result immediately and refreshes in
	// the background
	d.ttl = 0
	start := time.Now()
	if got := d.DetectAll(); len(got) != 1 {
		t.Errorf("stale DetectAll = %v", got)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Error("stale DetectAll blocked on the refresh")
	}
	deadline := time.Now().Add(5 * time.Second)
	for countLines(t, marker) < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := countLines(t, marker); n != 3 {
		t.Errorf("probe ran %d times after TTL expiry, want 3", n)
	}
}

func TestDetectAll_PreservesConfigOrderAndSkipsInvalidNames(t *testing.T) {
	d := newTestDetector(t,
		config.ToolConfig{Name: "hqssh-b", Command: "hqssh-b", Detect: "true"},
		config.ToolConfig{Name: "bad name!", Command: "x", Detect: "true"},
		config.ToolConfig{Name: "hqssh-a", Command: "hqssh-a", Detect: "true"},
		config.ToolConfig{Name: "hqssh-missing", Command: "hqssh-missing", Detect: "false"},
	)
	got := d.DetectAll()
	if strings.Join(got, ",") != "hqssh-b,hqssh-a" {
		t.Errorf("DetectAll = %v, want [hqssh-b hqssh-a] in config order", got)
	}
	if !d.IsInstalled("hqssh-a") || d.IsInstalled("hqssh-missing") || d.IsInstalled("unknown") {
		t.Error("IsInstalled disagrees with DetectAll")
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// A tool whose directory is added to PATH only by interactive shell init
// (~/.zshrc; ~/.bashrc behind the stock "not interactive → return" guard)
// must be detected, because sessions launch tools in an interactive login
// shell and would run it fine. Regression: zsh -l never read ~/.zshrc, so
// nvm/bun/volta installs showed "No AI tools" on macOS.
func TestDetectAll_FindsToolOnInteractiveInitPath(t *testing.T) {
	cases := []struct {
		shell string
		files func(binDir string) map[string]string
	}{
		{"/bin/zsh", func(binDir string) map[string]string {
			return map[string]string{
				".zshrc": "export PATH=\"" + binDir + ":$PATH\"\n",
			}
		}},
		{"/bin/bash", func(binDir string) map[string]string {
			return map[string]string{
				".bash_profile": "[ -f ~/.bashrc ] && . ~/.bashrc\n",
				".bashrc": "case $- in *i*) ;; *) return;; esac\n" +
					"export PATH=\"" + binDir + ":$PATH\"\n",
			}
		}},
	}
	for _, tc := range cases {
		t.Run(filepath.Base(tc.shell), func(t *testing.T) {
			if _, err := os.Stat(tc.shell); err != nil {
				t.Skipf("%s not installed", tc.shell)
			}
			home := t.TempDir()
			binDir := filepath.Join(home, "nvm-bin")
			if err := os.Mkdir(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			tool := filepath.Join(binDir, "hqssh-rc-tool")
			if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			for name, body := range tc.files(binDir) {
				if err := os.WriteFile(filepath.Join(home, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", home)
			t.Setenv("ZDOTDIR", home)
			t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")

			cfg := config.DefaultConfig()
			cfg.Tools = []config.ToolConfig{{Name: "hqssh-rc-tool", Command: "hqssh-rc-tool"}}
			d := NewDetector(cfg)
			t.Setenv("SHELL", tc.shell)

			if got := d.DetectAll(); len(got) != 1 || got[0] != "hqssh-rc-tool" {
				t.Fatalf("DetectAll() = %v, want [hqssh-rc-tool] (PATH set only in interactive init)", got)
			}
		})
	}
}
