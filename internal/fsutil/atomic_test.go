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

package fsutil

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func listTemps(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var temps []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			temps = append(temps, e.Name())
		}
	}
	return temps
}

func TestWriteFileAtomic_RoundTripAndPerm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")

	if err := WriteFileAtomic(path, []byte(`{"a":1}`), 0600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":1}` {
		t.Errorf("content = %q", got)
	}
	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("perm = %04o, want 0600", perm)
	}
	if temps := listTemps(t, dir); len(temps) != 0 {
		t.Errorf("temp files left behind: %v", temps)
	}

	// Overwrite keeps the file readable and still leaves no temps
	if err := WriteFileAtomic(path, []byte(`{"a":2}`), 0600); err != nil {
		t.Fatalf("second WriteFileAtomic: %v", err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != `{"a":2}` {
		t.Errorf("content after overwrite = %q", got)
	}
	if temps := listTemps(t, dir); len(temps) != 0 {
		t.Errorf("temp files left behind after overwrite: %v", temps)
	}
}

func TestWriteFileAtomic_FailureLeavesNoTemp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "ro")
	if err := os.Mkdir(target, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(target, 0700) })

	err := WriteFileAtomic(filepath.Join(target, "x.json"), []byte("data"), 0600)
	if err == nil {
		t.Fatal("expected error writing into a read-only directory")
	}
	if temps := listTemps(t, target); len(temps) != 0 {
		t.Errorf("temp files left behind: %v", temps)
	}
}

func TestRemoveStaleTemps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")

	keep := []string{"x.json", "y.json.tmp", "x.jsonfoo.tmp"}
	stale := []string{"x.json.tmp", "x.json.abc123.tmp"}
	for _, name := range append(append([]string{}, keep...), stale...) {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}

	removed := RemoveStaleTemps(path)
	sort.Strings(removed)
	sort.Strings(stale)
	if len(removed) != len(stale) {
		t.Fatalf("removed = %v, want %v", removed, stale)
	}
	for i := range stale {
		if removed[i] != stale[i] {
			t.Errorf("removed[%d] = %q, want %q", i, removed[i], stale[i])
		}
	}
	for _, name := range keep {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s should have been kept: %v", name, err)
		}
	}
	for _, name := range stale {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed", name)
		}
	}

	// Missing directory is not an error
	if got := RemoveStaleTemps(filepath.Join(dir, "nope", "x.json")); got != nil {
		t.Errorf("RemoveStaleTemps on missing dir = %v, want nil", got)
	}
}
