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

package task

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestNewStore(t *testing.T) {
	s := NewStore("/tmp/test")

	if s == nil {
		t.Fatal("NewStore returned nil")
	}
	if s.dataDir != "/tmp/test" {
		t.Errorf("dataDir = %q, want %q", s.dataDir, "/tmp/test")
	}
}

func TestStore_CreateAndGet(t *testing.T) {
	s := NewStore(t.TempDir())

	task := s.Create("test task", "description", "claude", TaskScopeProject, "proj-1", "do something", false, 300)

	if task == nil {
		t.Fatal("Create returned nil")
	}
	if task.ID == "" {
		t.Error("task.ID should not be empty")
	}
	if task.Name != "test task" {
		t.Errorf("task.Name = %q, want %q", task.Name, "test task")
	}
	if task.Description != "description" {
		t.Errorf("task.Description = %q, want %q", task.Description, "description")
	}
	if task.Tool != "claude" {
		t.Errorf("task.Tool = %q, want %q", task.Tool, "claude")
	}
	if task.Scope != TaskScopeProject {
		t.Errorf("task.Scope = %v, want %v", task.Scope, TaskScopeProject)
	}
	if task.ProjectID != "proj-1" {
		t.Errorf("task.ProjectID = %q, want %q", task.ProjectID, "proj-1")
	}
	if task.Prompt != "do something" {
		t.Errorf("task.Prompt = %q, want %q", task.Prompt, "do something")
	}
	if task.Interactive {
		t.Error("task.Interactive should be false")
	}
	if task.TimeoutSeconds != 300 {
		t.Errorf("task.TimeoutSeconds = %d, want 300", task.TimeoutSeconds)
	}

	// Get should return a copy with matching fields
	got := s.Get(task.ID)
	if got == nil || got.ID != task.ID || got.Name != task.Name || got.Tool != task.Tool {
		t.Errorf("Get() fields don't match: got %v, want %v", got, task)
	}
}

func TestStore_GetUnknown(t *testing.T) {
	s := NewStore(t.TempDir())

	task := s.Get("unknown-id")
	if task != nil {
		t.Errorf("Get(unknown) = %v, want nil", task)
	}
}

func TestStore_Update(t *testing.T) {
	s := NewStore(t.TempDir())

	task := s.Create("original", "", "shell", TaskScopeSystem, "", "echo hi", false, 0)
	originalUpdatedAt := task.UpdatedAt

	time.Sleep(10 * time.Millisecond)

	updated := s.Update(task.ID, "updated name", "new description", "claude", TaskScopeProject, "proj-1", "new prompt", true, 600)

	if updated == nil {
		t.Fatal("Update returned nil")
	}
	if updated.Name != "updated name" {
		t.Errorf("Name = %q, want %q", updated.Name, "updated name")
	}
	if updated.Description != "new description" {
		t.Errorf("Description = %q, want %q", updated.Description, "new description")
	}
	if updated.Tool != "claude" {
		t.Errorf("Tool = %q, want %q", updated.Tool, "claude")
	}
	if updated.Scope != TaskScopeProject {
		t.Errorf("Scope = %v, want %v", updated.Scope, TaskScopeProject)
	}
	if updated.ProjectID != "proj-1" {
		t.Errorf("ProjectID = %q, want %q", updated.ProjectID, "proj-1")
	}
	if updated.Prompt != "new prompt" {
		t.Errorf("Prompt = %q, want %q", updated.Prompt, "new prompt")
	}
	if !updated.Interactive {
		t.Error("Interactive should be true")
	}
	if updated.TimeoutSeconds != 600 {
		t.Errorf("TimeoutSeconds = %d, want 600", updated.TimeoutSeconds)
	}
	if !updated.UpdatedAt.After(originalUpdatedAt) {
		t.Error("UpdatedAt should be updated")
	}
}

func TestStore_UpdateUnknown(t *testing.T) {
	s := NewStore(t.TempDir())

	updated := s.Update("unknown-id", "name", "", "shell", TaskScopeSystem, "", "prompt", false, 0)
	if updated != nil {
		t.Errorf("Update(unknown) = %v, want nil", updated)
	}
}

func TestStore_Delete(t *testing.T) {
	s := NewStore(t.TempDir())

	task := s.Create("test", "", "shell", TaskScopeSystem, "", "echo", false, 0)

	deleted := s.Delete(task.ID)
	if !deleted {
		t.Error("Delete should return true")
	}

	// Should be gone
	got := s.Get(task.ID)
	if got != nil {
		t.Errorf("task should be deleted, got %v", got)
	}

	// Delete again should return false
	deleted = s.Delete(task.ID)
	if deleted {
		t.Error("Delete of non-existent should return false")
	}
}

func TestStore_ListAll(t *testing.T) {
	s := NewStore(t.TempDir())

	// Empty list
	tasks := s.List(TaskScopeUnspecified, "")
	if len(tasks) != 0 {
		t.Errorf("List() on empty store = %d, want 0", len(tasks))
	}

	// Create some tasks
	s.Create("task1", "", "shell", TaskScopeSystem, "", "echo 1", false, 0)
	s.Create("task2", "", "claude", TaskScopeProject, "proj-a", "do 2", false, 0)
	s.Create("task3", "", "aider", TaskScopeProject, "proj-b", "do 3", false, 0)

	// List all
	tasks = s.List(TaskScopeUnspecified, "")
	if len(tasks) != 3 {
		t.Errorf("List() = %d, want 3", len(tasks))
	}
}

func TestStore_ListByScope(t *testing.T) {
	s := NewStore(t.TempDir())

	s.Create("system1", "", "shell", TaskScopeSystem, "", "echo 1", false, 0)
	s.Create("system2", "", "shell", TaskScopeSystem, "", "echo 2", false, 0)
	s.Create("project1", "", "claude", TaskScopeProject, "proj-a", "do", false, 0)

	systemTasks := s.List(TaskScopeSystem, "")
	if len(systemTasks) != 2 {
		t.Errorf("List(System) = %d, want 2", len(systemTasks))
	}

	projectTasks := s.List(TaskScopeProject, "")
	if len(projectTasks) != 1 {
		t.Errorf("List(Project) = %d, want 1", len(projectTasks))
	}
}

func TestStore_ListByProject(t *testing.T) {
	s := NewStore(t.TempDir())

	s.Create("task1", "", "claude", TaskScopeProject, "proj-a", "do 1", false, 0)
	s.Create("task2", "", "codex", TaskScopeProject, "proj-a", "do 2", false, 0)
	s.Create("task3", "", "aider", TaskScopeProject, "proj-b", "do 3", false, 0)

	projATasks := s.List(TaskScopeUnspecified, "proj-a")
	if len(projATasks) != 2 {
		t.Errorf("List('', proj-a) = %d, want 2", len(projATasks))
	}

	projBTasks := s.List(TaskScopeUnspecified, "proj-b")
	if len(projBTasks) != 1 {
		t.Errorf("List('', proj-b) = %d, want 1", len(projBTasks))
	}
}

func TestStore_ListByScopeAndProject(t *testing.T) {
	s := NewStore(t.TempDir())

	s.Create("project-a-1", "", "claude", TaskScopeProject, "proj-a", "do", false, 0)
	s.Create("project-a-2", "", "codex", TaskScopeProject, "proj-a", "do", false, 0)
	s.Create("project-b-1", "", "aider", TaskScopeProject, "proj-b", "do", false, 0)
	s.Create("system-1", "", "shell", TaskScopeSystem, "", "echo", false, 0)

	// Filter by both scope and project
	tasks := s.List(TaskScopeProject, "proj-a")
	if len(tasks) != 2 {
		t.Errorf("List(Project, proj-a) = %d, want 2", len(tasks))
	}
}

func TestStore_Count(t *testing.T) {
	s := NewStore(t.TempDir())

	if s.Count() != 0 {
		t.Errorf("Count() on empty = %d, want 0", s.Count())
	}

	s.Create("task1", "", "shell", TaskScopeSystem, "", "echo", false, 0)
	s.Create("task2", "", "shell", TaskScopeSystem, "", "echo", false, 0)

	if s.Count() != 2 {
		t.Errorf("Count() = %d, want 2", s.Count())
	}
}

func TestStore_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	// Create some tasks
	task1 := s.Create("task1", "desc1", "claude", TaskScopeProject, "proj-a", "prompt1", false, 300)
	task2 := s.Create("task2", "desc2", "shell", TaskScopeSystem, "", "prompt2", true, 600)

	// Save
	if err := s.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Verify file exists
	path := filepath.Join(dir, tasksFile)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatalf("tasks file not created")
	}

	// Create new store and load
	s2 := NewStore(dir)
	if err := s2.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Verify tasks were loaded
	if s2.Count() != 2 {
		t.Errorf("loaded Count() = %d, want 2", s2.Count())
	}

	loaded1 := s2.Get(task1.ID)
	if loaded1 == nil {
		t.Fatal("task1 not loaded")
	}
	if loaded1.Name != "task1" {
		t.Errorf("loaded1.Name = %q, want %q", loaded1.Name, "task1")
	}
	if loaded1.Tool != "claude" {
		t.Errorf("loaded1.Tool = %q, want %q", loaded1.Tool, "claude")
	}

	loaded2 := s2.Get(task2.ID)
	if loaded2 == nil {
		t.Fatal("task2 not loaded")
	}
	if loaded2.Name != "task2" {
		t.Errorf("loaded2.Name = %q, want %q", loaded2.Name, "task2")
	}
}

func TestStore_LoadNonexistent(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	// Should not error on non-existent file
	if err := s.Load(); err != nil {
		t.Errorf("Load() on non-existent file = %v, want nil", err)
	}
	if s.Count() != 0 {
		t.Errorf("Count() after Load non-existent = %d, want 0", s.Count())
	}
}

func TestStore_ConcurrentAccess(t *testing.T) {
	s := NewStore(t.TempDir())

	var wg sync.WaitGroup

	// Concurrent creates
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			s.Create("task", "", "shell", TaskScopeSystem, "", "echo", false, 0)
		}(i)
	}

	// Concurrent reads
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.List(TaskScopeUnspecified, "")
			s.Count()
		}()
	}

	wg.Wait()

	if s.Count() != 10 {
		t.Errorf("Count() = %d, want 10", s.Count())
	}
}

func TestGenerateTaskID(t *testing.T) {
	now := time.Now()

	// Same inputs should give same ID
	id1 := generateTaskID("name", "proj", now)
	id2 := generateTaskID("name", "proj", now)
	if id1 != id2 {
		t.Errorf("same inputs should give same ID: %s != %s", id1, id2)
	}

	// Different inputs should give different IDs
	id3 := generateTaskID("name", "proj2", now)
	if id1 == id3 {
		t.Errorf("different inputs should give different IDs")
	}
}
