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
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gelotto/hqsshd/internal/fsutil"
)

const (
	runsFile = "task_runs.json"
)

// RunStatus represents the status of a task run
type RunStatus int

const (
	RunStatusUnspecified RunStatus = iota
	RunStatusPending
	RunStatusRunning
	RunStatusCompleted
	RunStatusFailed
	RunStatusCancelled
)

// Run represents a single execution of a task
type Run struct {
	ID          string    `json:"id"`
	TaskID      string    `json:"task_id"`
	Status      RunStatus `json:"status"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
	Output      string    `json:"output,omitempty"`
	Error       string    `json:"error,omitempty"`
	SessionID   string    `json:"session_id,omitempty"` // For interactive tasks
}

// RunStore manages task run history
type RunStore struct {
	runs           map[string]*Run   // All runs by ID
	byTask         map[string][]*Run // Runs indexed by task ID
	mu             sync.RWMutex
	dataDir        string
	maxRunsPerTask int // Max retained runs per task
}

// NewRunStore creates a new run store.
// maxRunsPerTask limits retained runs per task (0 = default 10).
func NewRunStore(dataDir string, maxRunsPerTask int) *RunStore {
	if maxRunsPerTask <= 0 {
		maxRunsPerTask = 10
	}
	return &RunStore{
		runs:           make(map[string]*Run),
		byTask:         make(map[string][]*Run),
		dataDir:        dataDir,
		maxRunsPerTask: maxRunsPerTask,
	}
}

// Load loads runs from disk
func (s *RunStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := filepath.Join(s.dataDir, runsFile)
	fsutil.RemoveStaleTemps(path)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var runs []*Run
	if err := json.Unmarshal(data, &runs); err != nil {
		return err
	}

	s.runs = make(map[string]*Run)
	s.byTask = make(map[string][]*Run)

	for _, r := range runs {
		s.runs[r.ID] = r
		s.byTask[r.TaskID] = append(s.byTask[r.TaskID], r)
	}

	// Sort runs by started_at for each task
	for taskID := range s.byTask {
		s.sortRuns(taskID)
	}

	return nil
}

// Save persists runs to disk
func (s *RunStore) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if err := os.MkdirAll(s.dataDir, 0700); err != nil {
		return err
	}

	runs := make([]*Run, 0, len(s.runs))
	for _, r := range s.runs {
		runs = append(runs, r)
	}

	data, err := json.MarshalIndent(runs, "", "  ")
	if err != nil {
		return err
	}

	// Atomic write (temp file + rename); temp is cleaned up on failure
	return fsutil.WriteFileAtomic(filepath.Join(s.dataDir, runsFile), data, 0600)
}

// Create creates a new run in pending state
func (s *RunStore) Create(taskID string) *Run {
	s.mu.Lock()
	defer s.mu.Unlock()

	run := &Run{
		ID:        uuid.New().String(),
		TaskID:    taskID,
		Status:    RunStatusPending,
		StartedAt: time.Now(),
	}

	s.runs[run.ID] = run
	s.byTask[taskID] = append(s.byTask[taskID], run)
	s.sortRuns(taskID)

	// Prune old runs for this task
	s.pruneRuns(taskID)

	return run
}

// Get retrieves a run by ID (returns a copy to prevent mutation of internal state)
func (s *RunStore) Get(runID string) *Run {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.runs[runID]
	if r == nil {
		return nil
	}
	copy := *r
	return &copy
}

// UpdateStatus updates the status of a run
func (s *RunStore) UpdateStatus(runID string, status RunStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if run, exists := s.runs[runID]; exists {
		run.Status = status
		if status == RunStatusCompleted || status == RunStatusFailed || status == RunStatusCancelled {
			run.CompletedAt = time.Now()
		}
	}
}

// SetOutput sets the output of a run
func (s *RunStore) SetOutput(runID, output string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if run, exists := s.runs[runID]; exists {
		run.Output = output
	}
}

// SetError sets the error of a run
func (s *RunStore) SetError(runID, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if run, exists := s.runs[runID]; exists {
		run.Error = errMsg
	}
}

// SetSessionID sets the session ID for an interactive run
func (s *RunStore) SetSessionID(runID, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if run, exists := s.runs[runID]; exists {
		run.SessionID = sessionID
	}
}

// Complete marks a run as completed with output
func (s *RunStore) Complete(runID, output string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if run, exists := s.runs[runID]; exists {
		run.Status = RunStatusCompleted
		run.Output = output
		run.CompletedAt = time.Now()
	}
}

// Fail marks a run as failed with error
func (s *RunStore) Fail(runID, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if run, exists := s.runs[runID]; exists {
		run.Status = RunStatusFailed
		run.Error = errMsg
		run.CompletedAt = time.Now()
	}
}

// Cancel marks a run as cancelled
func (s *RunStore) Cancel(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if run, exists := s.runs[runID]; exists {
		// Can only cancel pending or running tasks
		if run.Status == RunStatusPending || run.Status == RunStatusRunning {
			run.Status = RunStatusCancelled
			run.CompletedAt = time.Now()
			return true
		}
	}
	return false
}

// ListByTask returns runs for a specific task (returns copies to prevent mutation)
func (s *RunStore) ListByTask(taskID string, limit int) []*Run {
	s.mu.RLock()
	defer s.mu.RUnlock()

	runs := s.byTask[taskID]
	if runs == nil {
		return []*Run{}
	}

	// Return most recent first (already sorted)
	if limit <= 0 || limit > len(runs) {
		limit = len(runs)
	}

	result := make([]*Run, limit)
	for i := 0; i < limit; i++ {
		cp := *runs[i]
		result[i] = &cp
	}
	return result
}

// ListAll returns all runs, optionally limited (returns copies to prevent mutation)
func (s *RunStore) ListAll(limit int) []*Run {
	s.mu.RLock()
	defer s.mu.RUnlock()

	runs := make([]*Run, 0, len(s.runs))
	for _, r := range s.runs {
		cp := *r
		runs = append(runs, &cp)
	}

	// Sort by started_at descending
	sort.Slice(runs, func(i, j int) bool {
		return runs[i].StartedAt.After(runs[j].StartedAt)
	})

	if limit > 0 && limit < len(runs) {
		runs = runs[:limit]
	}

	return runs
}

// GetRunning returns the currently running run for a task, if any
// (returns a copy to prevent mutation of internal state)
func (s *RunStore) GetRunning(taskID string) *Run {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, r := range s.byTask[taskID] {
		if r.Status == RunStatusRunning {
			copy := *r
			return &copy
		}
	}
	return nil
}

// DeleteByTask removes all runs for a task
func (s *RunStore) DeleteByTask(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	runs := s.byTask[taskID]
	for _, r := range runs {
		delete(s.runs, r.ID)
	}
	delete(s.byTask, taskID)
}

// sortRuns sorts runs for a task by started_at descending (most recent first)
func (s *RunStore) sortRuns(taskID string) {
	runs := s.byTask[taskID]
	sort.Slice(runs, func(i, j int) bool {
		return runs[i].StartedAt.After(runs[j].StartedAt)
	})
}

// pruneRuns removes old runs beyond maxRunsPerTask
func (s *RunStore) pruneRuns(taskID string) {
	runs := s.byTask[taskID]
	if len(runs) <= s.maxRunsPerTask {
		return
	}

	// Keep only the most recent runs
	toRemove := runs[s.maxRunsPerTask:]
	s.byTask[taskID] = runs[:s.maxRunsPerTask]

	for _, r := range toRemove {
		delete(s.runs, r.ID)
	}
}
