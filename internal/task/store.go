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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const tasksFile = "tasks.json"

// TaskScope defines where a task can run
type TaskScope int

const (
	TaskScopeUnspecified TaskScope = iota
	TaskScopeGlobal                // Runs on any system (not implemented for MVP)
	TaskScopeSystem                // Runs on this system, no project context
	TaskScopeProject               // Runs in project directory
)

// Task represents a task definition
type Task struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	Tool           string    `json:"tool"` // claude, codex, aider
	Scope          TaskScope `json:"scope"`
	ProjectID      string    `json:"project_id,omitempty"` // Required if scope == PROJECT
	Prompt         string    `json:"prompt"`
	Interactive    bool      `json:"interactive"` // Not implemented for MVP
	TimeoutSeconds int       `json:"timeout_seconds"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Store manages task definitions with JSON persistence
type Store struct {
	tasks   map[string]*Task
	mu      sync.RWMutex
	dataDir string
}

// NewStore creates a new task store
func NewStore(dataDir string) *Store {
	return &Store{
		tasks:   make(map[string]*Task),
		dataDir: dataDir,
	}
}

// Load loads tasks from disk
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := filepath.Join(s.dataDir, tasksFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Empty store is fine
		}
		return err
	}

	var tasks []*Task
	if err := json.Unmarshal(data, &tasks); err != nil {
		return err
	}

	s.tasks = make(map[string]*Task)
	for _, t := range tasks {
		s.tasks[t.ID] = t
	}

	return nil
}

// Save persists tasks to disk
func (s *Store) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Ensure data directory exists
	if err := os.MkdirAll(s.dataDir, 0755); err != nil {
		return err
	}

	tasks := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		tasks = append(tasks, t)
	}

	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return err
	}

	// Atomic write: write to temp file then rename to avoid corruption on crash
	path := filepath.Join(s.dataDir, tasksFile)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// Create creates a new task
func (s *Store) Create(name, description, tool string, scope TaskScope, projectID, prompt string, interactive bool, timeoutSeconds int) *Task {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	task := &Task{
		ID:             generateTaskID(name, projectID, now),
		Name:           name,
		Description:    description,
		Tool:           tool,
		Scope:          scope,
		ProjectID:      projectID,
		Prompt:         prompt,
		Interactive:    interactive,
		TimeoutSeconds: timeoutSeconds,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	s.tasks[task.ID] = task
	return task
}

// Get retrieves a task by ID (returns a copy to prevent mutation of internal state)
func (s *Store) Get(id string) *Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.tasks[id]
	if t == nil {
		return nil
	}
	cp := *t
	return &cp
}

// Update updates an existing task
func (s *Store) Update(id, name, description, tool string, scope TaskScope, projectID, prompt string, interactive bool, timeoutSeconds int) *Task {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, exists := s.tasks[id]
	if !exists {
		return nil
	}

	task.Name = name
	task.Description = description
	task.Tool = tool
	task.Scope = scope
	task.ProjectID = projectID
	task.Prompt = prompt
	task.Interactive = interactive
	task.TimeoutSeconds = timeoutSeconds
	task.UpdatedAt = time.Now()

	return task
}

// Delete removes a task by ID
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.tasks[id]; exists {
		delete(s.tasks, id)
		return true
	}
	return false
}

// List returns all tasks, optionally filtered by scope and/or project
// (returns copies to prevent mutation of internal state)
func (s *Store) List(scope TaskScope, projectID string) []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tasks := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		// Filter by scope if specified
		if scope != TaskScopeUnspecified && t.Scope != scope {
			continue
		}

		// Filter by project if specified
		if projectID != "" && t.ProjectID != projectID {
			continue
		}

		cp := *t
		tasks = append(tasks, &cp)
	}

	return tasks
}

// Count returns the number of tasks
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tasks)
}

// generateTaskID generates a unique ID for a task
func generateTaskID(name, projectID string, created time.Time) string {
	data := name + projectID + created.String()
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:8]) // First 16 hex chars
}
