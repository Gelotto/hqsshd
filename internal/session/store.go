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

package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/gelotto/hqsshd/internal/logging"
)

const sessionsFile = "sessions.json"

// SessionRecord represents a persistent session record
type SessionRecord struct {
	ID               string    `json:"id"`
	Name             string    `json:"name,omitempty"`
	ProjectID        string    `json:"project_id,omitempty"`
	Tool             string    `json:"tool"`
	WorkingDirectory string    `json:"working_directory"`
	Command          string    `json:"command,omitempty"` // Full command with args
	Created          time.Time `json:"created"`
	Ended            time.Time `json:"ended,omitempty"`
	Status           string    `json:"status"` // "running", "idle", "ended"
	LogPath          string    `json:"log_path"`
}

// Store manages session metadata with JSON persistence
type Store struct {
	records  map[string]*SessionRecord
	mu       sync.RWMutex
	dataDir  string
	logDir   string
}

// NewStore creates a new session store
func NewStore(dataDir, logDir string) *Store {
	return &Store{
		records: make(map[string]*SessionRecord),
		dataDir: dataDir,
		logDir:  logDir,
	}
}

// Load loads session records from disk
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := filepath.Join(s.dataDir, sessionsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Empty store is fine
		}
		return err
	}

	var records []*SessionRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return err
	}

	s.records = make(map[string]*SessionRecord)
	for _, r := range records {
		s.records[r.ID] = r
	}

	logging.Info("session store loaded",
		"records", len(s.records),
	)

	return nil
}

// Save persists session records to disk
func (s *Store) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Ensure data directory exists (0700: owner-only access)
	if err := os.MkdirAll(s.dataDir, 0700); err != nil {
		return err
	}

	records := make([]*SessionRecord, 0, len(s.records))
	for _, r := range s.records {
		records = append(records, r)
	}

	// Sort by created time (newest first) for readability
	sort.Slice(records, func(i, j int) bool {
		return records[i].Created.After(records[j].Created)
	})

	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}

	// Use atomic write: write to temp file, then rename
	// This prevents corruption if process crashes mid-write
	path := filepath.Join(s.dataDir, sessionsFile)
	tempPath := path + ".tmp"

	if err := os.WriteFile(tempPath, data, 0600); err != nil {
		return err
	}

	return os.Rename(tempPath, path)
}

// Add adds a new session record
func (s *Store) Add(record *SessionRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[record.ID] = record
}

// Get retrieves a session record by ID
func (s *Store) Get(id string) *SessionRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if r, ok := s.records[id]; ok {
		// Return a copy to avoid races
		copy := *r
		return &copy
	}
	return nil
}

// Update updates an existing session record
func (s *Store) Update(id string, update func(r *SessionRecord)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r, ok := s.records[id]; ok {
		update(r)
		return true
	}
	return false
}

// MarkEnded marks a session as ended with the current timestamp
func (s *Store) MarkEnded(id string) bool {
	return s.Update(id, func(r *SessionRecord) {
		r.Status = "ended"
		r.Ended = time.Now()
	})
}

// UpdateStatus updates the status of a session
func (s *Store) UpdateStatus(id, status string) bool {
	return s.Update(id, func(r *SessionRecord) {
		r.Status = status
	})
}

// Delete removes a session record by ID
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.records[id]; exists {
		delete(s.records, id)
		return true
	}
	return false
}

// List returns all session records, optionally filtered
func (s *Store) List(projectID string, includeEnded bool) []*SessionRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	records := make([]*SessionRecord, 0, len(s.records))
	for _, r := range s.records {
		// Filter by project if specified
		if projectID != "" && r.ProjectID != projectID {
			continue
		}

		// Filter out ended unless requested
		if !includeEnded && r.Status == "ended" {
			continue
		}

		// Return a copy
		copy := *r
		records = append(records, &copy)
	}

	// Sort by created time (newest first)
	sort.Slice(records, func(i, j int) bool {
		return records[i].Created.After(records[j].Created)
	})

	return records
}

// ListEnded returns only ended sessions (for historical view)
func (s *Store) ListEnded(limit int) []*SessionRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	records := make([]*SessionRecord, 0)
	for _, r := range s.records {
		if r.Status == "ended" {
			copy := *r
			records = append(records, &copy)
		}
	}

	// Sort by ended time (newest first)
	sort.Slice(records, func(i, j int) bool {
		return records[i].Ended.After(records[j].Ended)
	})

	// Apply limit
	if limit > 0 && len(records) > limit {
		records = records[:limit]
	}

	return records
}

// Cleanup removes records older than retentionDays and their log files
func (s *Store) Cleanup(retentionDays int) (removed int, err error) {
	if retentionDays <= 0 {
		return 0, nil // No cleanup if retention is disabled
	}

	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	toDelete := []string{}

	s.mu.Lock()
	for id, r := range s.records {
		// Only clean up ended sessions that are older than retention
		if r.Status == "ended" && !r.Ended.IsZero() && r.Ended.Before(cutoff) {
			toDelete = append(toDelete, id)
		}
	}

	for _, id := range toDelete {
		delete(s.records, id)
	}
	s.mu.Unlock()

	// Delete log files outside the lock
	for _, id := range toDelete {
		if err := DeleteLog(s.logDir, id); err != nil {
			logging.Warn("failed to delete session log",
				"session_id", id,
				"error", err,
			)
		}
	}

	if len(toDelete) > 0 {
		logging.Info("session store cleanup completed",
			"removed", len(toDelete),
			"retention_days", retentionDays,
		)

		// Save after cleanup
		if saveErr := s.Save(); saveErr != nil {
			logging.Warn("failed to save after cleanup",
				"error", saveErr,
			)
			return len(toDelete), saveErr
		}
	}

	return len(toDelete), nil
}

// Count returns the number of records
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.records)
}

// CountEnded returns the number of ended sessions
func (s *Store) CountEnded() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	count := 0
	for _, r := range s.records {
		if r.Status == "ended" {
			count++
		}
	}
	return count
}

// CreateRecord creates a SessionRecord from a Session
func CreateRecord(sess *Session, logDir string) *SessionRecord {
	// Build command string for display
	command := sess.Tool
	if len(sess.Args) > 0 {
		for _, arg := range sess.Args {
			command += " " + arg
		}
	}

	return &SessionRecord{
		ID:               sess.ID,
		Name:             sess.Name,
		ProjectID:        sess.ProjectID,
		Tool:             sess.Tool,
		WorkingDirectory: sess.WorkingDirectory,
		Command:          command,
		Created:          sess.CreatedAt,
		Status:           sess.Status().String(),
		LogPath:          filepath.Join(logDir, sess.ID+".log"),
	}
}
