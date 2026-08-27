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
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/gelotto/hqsshd/internal/fsutil"
)

const stateFile = "discovery.json"

// DiscoveryState persists discovery preferences that must survive restarts:
// directories the user has explicitly scanned (so automatic rescans cover
// them), and paths of projects the user deliberately removed (so automatic
// rescans never re-add them).
type DiscoveryState struct {
	mu      sync.RWMutex
	dataDir string
	roots   []string
	removed map[string]bool
}

type discoveryStateJSON struct {
	ScanRoots    []string `json:"scan_roots"`
	RemovedPaths []string `json:"removed_paths"`
}

// NewDiscoveryState creates a new discovery state store
func NewDiscoveryState(dataDir string) *DiscoveryState {
	return &DiscoveryState{
		dataDir: dataDir,
		removed: make(map[string]bool),
	}
}

// Load loads the state from disk
func (s *DiscoveryState) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := filepath.Join(s.dataDir, stateFile)
	fsutil.RemoveStaleTemps(path)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Empty state is fine
		}
		return err
	}

	var stored discoveryStateJSON
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}

	s.roots = stored.ScanRoots
	s.removed = make(map[string]bool, len(stored.RemovedPaths))
	for _, p := range stored.RemovedPaths {
		s.removed[p] = true
	}

	return nil
}

// Save persists the state to disk
func (s *DiscoveryState) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if err := os.MkdirAll(s.dataDir, 0700); err != nil {
		return err
	}

	stored := discoveryStateJSON{
		ScanRoots:    append([]string{}, s.roots...),
		RemovedPaths: make([]string, 0, len(s.removed)),
	}
	for p := range s.removed {
		stored.RemovedPaths = append(stored.RemovedPaths, p)
	}
	sort.Strings(stored.RemovedPaths)

	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}

	// Atomic write (temp file + rename); temp is cleaned up on failure
	return fsutil.WriteFileAtomic(filepath.Join(s.dataDir, stateFile), data, 0600)
}

// AddScanRoots records directories as persistent scan roots. Paths are
// home-expanded and cleaned; entries that don't exist or aren't directories
// are skipped. Returns true if the root set changed.
func (s *DiscoveryState) AddScanRoots(dirs []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	changed := false
	for _, dir := range dirs {
		dir = filepath.Clean(expandHome(dir))
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		exists := false
		for _, r := range s.roots {
			if r == dir {
				exists = true
				break
			}
		}
		if !exists {
			s.roots = append(s.roots, dir)
			changed = true
		}
	}
	return changed
}

// ScanRoots returns the persisted scan roots
func (s *DiscoveryState) ScanRoots() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string{}, s.roots...)
}

// AddRemoved records a project path as deliberately removed so rescans skip it
func (s *DiscoveryState) AddRemoved(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removed[filepath.Clean(path)] = true
}

// ClearRemoved forgets a removal tombstone (the user explicitly re-added the
// project). Returns true if a tombstone existed.
func (s *DiscoveryState) ClearRemoved(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	path = filepath.Clean(path)
	if s.removed[path] {
		delete(s.removed, path)
		return true
	}
	return false
}

// FilterRemoved drops projects whose paths carry a removal tombstone
func (s *DiscoveryState) FilterRemoved(projects []*Project) []*Project {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.removed) == 0 {
		return projects
	}
	kept := make([]*Project, 0, len(projects))
	for _, p := range projects {
		if !s.removed[filepath.Clean(p.Path)] {
			kept = append(kept, p)
		}
	}
	return kept
}
