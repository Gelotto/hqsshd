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
