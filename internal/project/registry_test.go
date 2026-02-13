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
	"sync"
	"testing"
	"time"
)

func TestNewRegistry(t *testing.T) {
	r := NewRegistry("/tmp/test")

	if r == nil {
		t.Fatal("NewRegistry returned nil")
	}
	if r.dataDir != "/tmp/test" {
		t.Errorf("dataDir = %q, want %q", r.dataDir, "/tmp/test")
	}
}

func TestRegistry_AddAndGet(t *testing.T) {
	r := NewRegistry(t.TempDir())

	project := &Project{
		ID:   "test-id",
		Name: "test-project",
		Path: "/path/to/project",
	}

	r.Add(project)

	got := r.Get("test-id")
	if got == nil || got.ID != project.ID || got.Name != project.Name || got.Path != project.Path {
		t.Errorf("Get() fields don't match: got %v, want %v", got, project)
	}
}

func TestRegistry_GetUnknown(t *testing.T) {
	r := NewRegistry(t.TempDir())

	got := r.Get("unknown-id")
	if got != nil {
		t.Errorf("Get(unknown) = %v, want nil", got)
	}
}

func TestRegistry_GetByPath(t *testing.T) {
	r := NewRegistry(t.TempDir())

	project := &Project{
		ID:   "test-id",
		Name: "test-project",
		Path: "/path/to/project",
	}
	r.Add(project)

	got := r.GetByPath("/path/to/project")
	if got == nil || got.ID != project.ID || got.Name != project.Name || got.Path != project.Path {
		t.Errorf("GetByPath() fields don't match: got %v, want %v", got, project)
	}

	// Unknown path
	unknown := r.GetByPath("/unknown/path")
	if unknown != nil {
		t.Errorf("GetByPath(unknown) = %v, want nil", unknown)
	}
}

func TestRegistry_Remove(t *testing.T) {
	r := NewRegistry(t.TempDir())

	project := &Project{
		ID:   "test-id",
		Name: "test-project",
		Path: "/path/to/project",
	}
	r.Add(project)

	// Remove should return true
	removed := r.Remove("test-id")
	if !removed {
		t.Error("Remove() should return true")
	}

	// Should be gone
	got := r.Get("test-id")
	if got != nil {
		t.Errorf("project should be removed, got %v", got)
	}

	// Remove again should return false
	removed = r.Remove("test-id")
	if removed {
		t.Error("Remove(nonexistent) should return false")
	}
}

func TestRegistry_List(t *testing.T) {
	r := NewRegistry(t.TempDir())

	// Empty list
	projects := r.List(false)
	if len(projects) != 0 {
		t.Errorf("List() on empty = %d, want 0", len(projects))
	}

	// Add some projects
	r.Add(&Project{ID: "p1", Name: "project1", IsFavorite: true})
	r.Add(&Project{ID: "p2", Name: "project2", IsFavorite: false})
	r.Add(&Project{ID: "p3", Name: "project3", IsFavorite: true})

	// List all
	all := r.List(false)
	if len(all) != 3 {
		t.Errorf("List(false) = %d, want 3", len(all))
	}

	// List favorites only
	favorites := r.List(true)
	if len(favorites) != 2 {
		t.Errorf("List(true) = %d, want 2", len(favorites))
	}
}

func TestRegistry_Count(t *testing.T) {
	r := NewRegistry(t.TempDir())

	if r.Count() != 0 {
		t.Errorf("Count() on empty = %d, want 0", r.Count())
	}

	r.Add(&Project{ID: "p1"})
	r.Add(&Project{ID: "p2"})

	if r.Count() != 2 {
		t.Errorf("Count() = %d, want 2", r.Count())
	}
}

func TestRegistry_UpdateLastAccessed(t *testing.T) {
	r := NewRegistry(t.TempDir())

	project := &Project{
		ID:           "test-id",
		LastAccessed: time.Time{},
	}
	r.Add(project)

	time.Sleep(10 * time.Millisecond)
	r.UpdateLastAccessed("test-id")

	got := r.Get("test-id")
	if got.LastAccessed.IsZero() {
		t.Error("LastAccessed should be updated")
	}
}

func TestRegistry_UpdateLastAccessed_Unknown(t *testing.T) {
	r := NewRegistry(t.TempDir())

	// Should not panic
	r.UpdateLastAccessed("unknown-id")
}

func TestRegistry_SetFavorite(t *testing.T) {
	r := NewRegistry(t.TempDir())

	project := &Project{
		ID:         "test-id",
		IsFavorite: false,
	}
	r.Add(project)

	// Set to favorite
	ok := r.SetFavorite("test-id", true)
	if !ok {
		t.Error("SetFavorite should return true")
	}

	got := r.Get("test-id")
	if !got.IsFavorite {
		t.Error("IsFavorite should be true")
	}

	// Set back to not favorite
	ok = r.SetFavorite("test-id", false)
	if !ok {
		t.Error("SetFavorite should return true")
	}

	got = r.Get("test-id")
	if got.IsFavorite {
		t.Error("IsFavorite should be false")
	}
}

func TestRegistry_SetFavorite_Unknown(t *testing.T) {
	r := NewRegistry(t.TempDir())

	ok := r.SetFavorite("unknown-id", true)
	if ok {
		t.Error("SetFavorite(unknown) should return false")
	}
}

func TestRegistry_MergeDiscovered(t *testing.T) {
	r := NewRegistry(t.TempDir())

	// Add existing project
	existing := &Project{
		ID:         "existing-id",
		Name:       "existing",
		IsFavorite: true,
	}
	r.Add(existing)

	// Merge with discovered
	discovered := []*Project{
		{ID: "existing-id", Name: "discovered-same", IsFavorite: false}, // Same ID
		{ID: "new-id-1", Name: "new1"},
		{ID: "new-id-2", Name: "new2"},
	}

	added := r.MergeDiscovered(discovered)

	if added != 2 {
		t.Errorf("MergeDiscovered() returned %d, want 2", added)
	}
	if r.Count() != 3 {
		t.Errorf("Count() = %d, want 3", r.Count())
	}

	// Existing project should be unchanged
	got := r.Get("existing-id")
	if got.Name != "existing" {
		t.Errorf("existing name = %q, should be unchanged", got.Name)
	}
	if !got.IsFavorite {
		t.Error("existing IsFavorite should be preserved")
	}
}

func TestRegistry_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(dir)

	// Add projects
	r.Add(&Project{ID: "p1", Name: "project1", Path: "/path/1", IsFavorite: true})
	r.Add(&Project{ID: "p2", Name: "project2", Path: "/path/2", IsFavorite: false})

	// Save
	if err := r.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Verify file exists
	path := filepath.Join(dir, registryFile)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatalf("registry file not created")
	}

	// Create new registry and load
	r2 := NewRegistry(dir)
	if err := r2.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Verify projects were loaded
	if r2.Count() != 2 {
		t.Errorf("loaded Count() = %d, want 2", r2.Count())
	}

	loaded1 := r2.Get("p1")
	if loaded1 == nil {
		t.Fatal("p1 not loaded")
	}
	if loaded1.Name != "project1" {
		t.Errorf("loaded1.Name = %q, want %q", loaded1.Name, "project1")
	}
	if !loaded1.IsFavorite {
		t.Error("loaded1.IsFavorite should be true")
	}
}

func TestRegistry_LoadNonexistent(t *testing.T) {
	r := NewRegistry(t.TempDir())

	// Should not error
	if err := r.Load(); err != nil {
		t.Errorf("Load() on nonexistent file = %v, want nil", err)
	}
	if r.Count() != 0 {
		t.Errorf("Count() after Load = %d, want 0", r.Count())
	}
}

func TestRegistry_ConcurrentAccess(t *testing.T) {
	r := NewRegistry(t.TempDir())

	var wg sync.WaitGroup

	// Concurrent adds
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r.Add(&Project{ID: string(rune('A' + n))})
		}(i)
	}

	// Concurrent reads
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.List(false)
			r.Count()
		}()
	}

	wg.Wait()

	if r.Count() != 10 {
		t.Errorf("Count() = %d, want 10", r.Count())
	}
}
