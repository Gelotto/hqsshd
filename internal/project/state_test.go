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

package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAddScanRootsDedupesAndSkipsMissing(t *testing.T) {
	dataDir := t.TempDir()
	root := t.TempDir()
	state := NewDiscoveryState(dataDir)

	if !state.AddScanRoots([]string{root}) {
		t.Fatal("expected adding a new root to report a change")
	}
	if state.AddScanRoots([]string{root}) {
		t.Fatal("expected duplicate root to report no change")
	}
	if state.AddScanRoots([]string{filepath.Join(root, "does-not-exist")}) {
		t.Fatal("expected missing directory to be skipped")
	}

	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if state.AddScanRoots([]string{file}) {
		t.Fatal("expected non-directory to be skipped")
	}

	roots := state.ScanRoots()
	if len(roots) != 1 || roots[0] != root {
		t.Fatalf("expected roots [%s], got %v", root, roots)
	}
}

func TestAddScanRootsExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sub := filepath.Join(home, "code")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}

	state := NewDiscoveryState(t.TempDir())
	if !state.AddScanRoots([]string{"~/code"}) {
		t.Fatal("expected ~/code to be added")
	}
	roots := state.ScanRoots()
	if len(roots) != 1 || roots[0] != sub {
		t.Fatalf("expected roots [%s], got %v", sub, roots)
	}
}

func TestRemovedTombstonesFilterAndClear(t *testing.T) {
	state := NewDiscoveryState(t.TempDir())
	projects := []*Project{
		{ID: "a", Path: "/home/u/code/keep"},
		{ID: "b", Path: "/home/u/code/gone"},
	}

	state.AddRemoved("/home/u/code/gone")

	kept := state.FilterRemoved(projects)
	if len(kept) != 1 || kept[0].ID != "a" {
		t.Fatalf("expected only project a to survive, got %v", kept)
	}

	if !state.ClearRemoved("/home/u/code/gone") {
		t.Fatal("expected ClearRemoved to report an existing tombstone")
	}
	if state.ClearRemoved("/home/u/code/gone") {
		t.Fatal("expected second ClearRemoved to report no tombstone")
	}
	if kept := state.FilterRemoved(projects); len(kept) != 2 {
		t.Fatalf("expected both projects after clearing tombstone, got %v", kept)
	}
}

func TestDiscoveryStatePersistence(t *testing.T) {
	dataDir := t.TempDir()
	root := t.TempDir()

	state := NewDiscoveryState(dataDir)
	state.AddScanRoots([]string{root})
	state.AddRemoved("/home/u/code/gone")
	if err := state.Save(); err != nil {
		t.Fatal(err)
	}

	reloaded := NewDiscoveryState(dataDir)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}

	roots := reloaded.ScanRoots()
	if len(roots) != 1 || roots[0] != root {
		t.Fatalf("expected roots [%s] after reload, got %v", root, roots)
	}
	kept := reloaded.FilterRemoved([]*Project{{ID: "b", Path: "/home/u/code/gone"}})
	if len(kept) != 0 {
		t.Fatalf("expected tombstone to survive reload, got %v", kept)
	}
}

func TestDiscoveryStateLoadMissingFile(t *testing.T) {
	state := NewDiscoveryState(t.TempDir())
	if err := state.Load(); err != nil {
		t.Fatalf("expected missing state file to load cleanly, got %v", err)
	}
	if len(state.ScanRoots()) != 0 {
		t.Fatal("expected empty roots for fresh state")
	}
}
