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
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gelotto/hqsshd/internal/logging"
	"github.com/google/uuid"
)

// Manager manages all active sessions
type Manager struct {
	sessions   map[string]*Session
	sessionsMu sync.RWMutex

	// Configuration
	idleTimeout      time.Duration
	maxSessions      int
	maxBufferSize    int // bytes for scrollback buffer
	clientBufferSize int // per-client output channel buffer size
	cleanupTicker    *time.Ticker
	done             chan struct{}
	wg               sync.WaitGroup // Wait for cleanupLoop to exit
	closed           atomic.Bool    // Prevent double-close panic

	// Persistence
	store        *Store // Session metadata persistence
	logDir       string // Directory for session logs
	retentionDays int   // Days to keep ended session logs (0 = forever)

	// Event fan-out for watchers (mobile app notifications)
	events *EventHub
}

// NewManager creates a new session manager
// historySize is in lines - we estimate ~100 bytes per line for buffer sizing
// dataDir is the base data directory (e.g., ~/.hqssh)
// logDir is the directory for session logs (empty string uses default: dataDir/logs/sessions)
// retentionDays is how long to keep ended session logs (0 = forever)
// clientBufferSize is the per-client output channel buffer size (0 = default 256)
// maxScrollbackSize overrides the scrollback buffer size in bytes (0 = derive from historySize)
func NewManager(idleTimeoutSec, maxSessions, historySize int, dataDir, logDir string, retentionDays, clientBufferSize, maxScrollbackSize int) *Manager {
	// Use explicit maxScrollbackSize if provided, otherwise derive from historySize
	maxBufferSize := maxScrollbackSize
	if maxBufferSize <= 0 {
		// Convert lines to bytes (estimate ~100 bytes per line)
		maxBufferSize = historySize * 100
		if maxBufferSize <= 0 {
			// 10MB default - AI tools (Claude, Codex, etc.) produce lots of output
			// with syntax highlighting, markdown, and verbose responses
			maxBufferSize = 10 * 1024 * 1024
		}
	}

	if clientBufferSize <= 0 {
		clientBufferSize = 256
	}

	// Default log directory
	if logDir == "" {
		logDir = dataDir + "/logs/sessions"
	}

	// Create session store
	store := NewStore(dataDir, logDir)
	if err := store.Load(); err != nil {
		logging.Warn("failed to load session store", "error", err)
	}

	m := &Manager{
		sessions:         make(map[string]*Session),
		idleTimeout:      time.Duration(idleTimeoutSec) * time.Second,
		maxSessions:      maxSessions,
		maxBufferSize:    maxBufferSize,
		clientBufferSize: clientBufferSize,
		done:             make(chan struct{}),
		store:            store,
		logDir:           logDir,
		retentionDays:    retentionDays,
		events:           NewEventHub(),
	}

	// Mark any previously "running" or "idle" sessions as ended (daemon restart)
	for _, record := range store.List("", true) {
		if record.Status == "running" || record.Status == "idle" {
			if !store.MarkEnded(record.ID) {
				logging.Warn("failed to mark stale session as ended",
					"session_id", record.ID,
				)
			}
		}
	}
	if err := store.Save(); err != nil {
		logging.Warn("failed to save session store after marking stale sessions", "error", err)
	}

	// Start cleanup routine
	m.cleanupTicker = time.NewTicker(1 * time.Minute)
	m.wg.Add(1)
	go m.cleanupLoop()

	return m
}

// Events returns the manager's session event hub for watchers
func (m *Manager) Events() *EventHub {
	return m.events
}

// cleanupLoop periodically cleans up ended or idle sessions
func (m *Manager) cleanupLoop() {
	defer m.wg.Done()
	for {
		select {
		case <-m.cleanupTicker.C:
			m.cleanup()
		case <-m.done:
			return
		}
	}
}

// cleanup removes ended sessions and checks for idle timeouts
func (m *Manager) cleanup() {
	now := time.Now()

	// Phase 1: Identify sessions to close while holding the lock briefly
	m.sessionsMu.Lock()

	type sessionToClose struct {
		id   string
		sess *Session
	}
	var toClose []sessionToClose
	var endedCount, idleTimeoutCount int

	for id, sess := range m.sessions {
		status := sess.Status()

		if status == StatusEnded {
			toClose = append(toClose, sessionToClose{id, sess})
			endedCount++
			continue
		}

		if m.idleTimeout > 0 && status == StatusIdle {
			idleFor := now.Sub(sess.LastActivity())
			if idleFor > m.idleTimeout {
				logging.Info("session idle timeout",
					"session_id", id,
					"tool", sess.Tool,
					"idle_duration", idleFor.String(),
					"idle_timeout", m.idleTimeout.String(),
				)
				toClose = append(toClose, sessionToClose{id, sess})
				idleTimeoutCount++
			}
		}
	}

	// Remove from map while we have the lock
	for _, sc := range toClose {
		delete(m.sessions, sc.id)
	}
	remaining := len(m.sessions)

	m.sessionsMu.Unlock()

	// Phase 2: Close sessions without holding the lock (Close() can block)
	for _, sc := range toClose {
		sc.sess.Close()
		if !m.store.MarkEnded(sc.id) {
			logging.Warn("failed to mark session as ended in store",
				"session_id", sc.id,
			)
		}
	}

	// Log cleanup summary if any sessions were removed
	if len(toClose) > 0 {
		logging.Debug("cleanup completed",
			"removed_ended", endedCount,
			"removed_idle_timeout", idleTimeoutCount,
			"remaining_sessions", remaining,
		)

		// Save store after marking sessions as ended
		if err := m.store.Save(); err != nil {
			logging.Warn("failed to save session store after cleanup", "error", err)
		}
	}

	// Run log retention cleanup
	if m.retentionDays > 0 {
		removed, err := m.store.Cleanup(m.retentionDays)
		if err != nil {
			logging.Warn("session log retention cleanup failed", "error", err)
		} else if removed > 0 {
			logging.Info("session log retention cleanup completed",
				"removed", removed,
				"retention_days", m.retentionDays,
			)
			if err := m.store.Save(); err != nil {
				logging.Warn("failed to save session store after retention cleanup", "error", err)
			}
		}
	}
}

// Create creates a new session with the given parameters.
// If name is empty, it is auto-generated from workingDir, tool, and short ID.
func (m *Manager) Create(projectID, tool, workingDir, name string, args []string, cols, rows int) (*Session, error) {
	m.sessionsMu.Lock()
	defer m.sessionsMu.Unlock()

	// Reject creates racing shutdown: Close() sets the flag before taking
	// sessionsMu, so checking under the lock means a session is either
	// rejected here or included in Close()'s sweep -- never leaked
	if m.closed.Load() {
		return nil, fmt.Errorf("session manager is shut down")
	}

	// Check max sessions limit
	if m.maxSessions > 0 && len(m.sessions) >= m.maxSessions {
		logging.Warn("max sessions limit reached",
			"max_sessions", m.maxSessions,
			"current_sessions", len(m.sessions),
		)
		return nil, fmt.Errorf("maximum sessions limit (%d) reached", m.maxSessions)
	}

	// Create session with config-based buffer size
	sess := NewSession(projectID, tool, workingDir, name, args, cols, rows, m.maxBufferSize)
	sess.SetEventCallback(m.events.Publish)

	// Create session logger for persistent output
	logger, err := NewSessionLogger(sess.ID, m.logDir)
	if err != nil {
		logging.Warn("failed to create session logger",
			"session_id", sess.ID,
			"error", err,
		)
		// Continue without logger - session will still work, just no persistence
	} else {
		sess.SetLogger(logger)
	}

	// Start PTY
	if err := sess.StartPTY(); err != nil {
		// Clean up logger if PTY fails
		if logger != nil {
			logger.Close()
		}
		return nil, fmt.Errorf("failed to start PTY: %w", err)
	}

	// Store session in memory
	m.sessions[sess.ID] = sess

	// Add to persistent store
	record := CreateRecord(sess, m.logDir)
	m.store.Add(record)
	if err := m.store.Save(); err != nil {
		logging.Warn("failed to save session store",
			"session_id", sess.ID,
			"error", err,
		)
	}

	logging.Info("session registered",
		"session_id", sess.ID,
		"tool", tool,
		"project_id", projectID,
		"total_sessions", len(m.sessions),
		"log_path", record.LogPath,
	)

	return sess, nil
}

// Get retrieves a session by ID
func (m *Manager) Get(sessionID string) *Session {
	m.sessionsMu.RLock()
	defer m.sessionsMu.RUnlock()
	return m.sessions[sessionID]
}

// GetScrollback returns the scrollback buffer for a session
func (m *Manager) GetScrollback(sessionID string) ([]byte, error) {
	sess := m.Get(sessionID)
	if sess == nil {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}
	return sess.GetScrollback(), nil
}

// List returns all active sessions, optionally filtered by project
func (m *Manager) List(projectID string, includeEnded bool) []*Session {
	m.sessionsMu.RLock()
	defer m.sessionsMu.RUnlock()

	result := make([]*Session, 0, len(m.sessions))

	for _, sess := range m.sessions {
		// Filter by project if specified
		if projectID != "" && sess.ProjectID != projectID {
			continue
		}

		// Filter out ended unless requested
		if !includeEnded && sess.Status() == StatusEnded {
			continue
		}

		result = append(result, sess)
	}

	return result
}

// Attach attaches a client to a session and returns the output channel
func (m *Manager) Attach(sessionID string, cols, rows int) (string, <-chan []byte, []byte, error) {
	m.sessionsMu.RLock()
	sess := m.sessions[sessionID]
	m.sessionsMu.RUnlock()

	if sess == nil {
		logging.Debug("attach failed: session not found", "session_id", sessionID)
		return "", nil, nil, fmt.Errorf("session not found: %s", sessionID)
	}

	if sess.IsDone() {
		logging.Debug("attach failed: session ended", "session_id", sessionID)
		return "", nil, nil, fmt.Errorf("session has ended: %s", sessionID)
	}

	// Generate client ID
	clientID := uuid.New().String()

	// Resize if needed
	if cols > 0 && rows > 0 {
		if err := sess.Resize(cols, rows); err != nil {
			logging.Warn("failed to resize session",
				"session_id", sessionID,
				"cols", cols,
				"rows", rows,
				"error", err,
			)
		}
	}

	// Atomically snapshot replay bytes (mode preamble + scrollback) and
	// register the client so no PTY output falls between the two
	scrollback, outputCh := sess.AttachClient(clientID, m.clientBufferSize)

	// Ask the process to repaint now that the client is registered — its
	// redraw broadcasts after the replay snapshot, giving the client a
	// fresh frame even when the attach resize was a same-size no-op.
	sess.SignalRepaint()

	logging.Info("client attached",
		"session_id", sessionID,
		"client_id", clientID,
		"scrollback_bytes", len(scrollback),
	)

	return clientID, outputCh, scrollback, nil
}

// Detach removes a client from a session
func (m *Manager) Detach(sessionID, clientID string) error {
	m.sessionsMu.RLock()
	sess := m.sessions[sessionID]
	m.sessionsMu.RUnlock()

	if sess == nil {
		logging.Debug("detach failed: session not found",
			"session_id", sessionID,
			"client_id", clientID,
		)
		return fmt.Errorf("session not found: %s", sessionID)
	}

	sess.RemoveClient(clientID)

	logging.Info("client detached",
		"session_id", sessionID,
		"client_id", clientID,
	)

	return nil
}

// Input sends input to a session
func (m *Manager) Input(sessionID string, data []byte) error {
	m.sessionsMu.RLock()
	sess := m.sessions[sessionID]
	m.sessionsMu.RUnlock()

	if sess == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	if sess.IsDone() {
		return fmt.Errorf("session has ended: %s", sessionID)
	}

	_, err := sess.Write(data)
	return err
}

// Resize resizes a session's terminal
func (m *Manager) Resize(sessionID string, cols, rows int) error {
	m.sessionsMu.RLock()
	sess := m.sessions[sessionID]
	m.sessionsMu.RUnlock()

	if sess == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	return sess.Resize(cols, rows)
}

// Kill terminates a session and removes it from the map
func (m *Manager) Kill(sessionID string) error {
	m.sessionsMu.Lock()
	sess := m.sessions[sessionID]
	if sess == nil {
		m.sessionsMu.Unlock()
		logging.Debug("kill failed: session not found", "session_id", sessionID)
		return fmt.Errorf("session not found: %s", sessionID)
	}
	// Remove from map immediately
	delete(m.sessions, sessionID)
	remaining := len(m.sessions)
	m.sessionsMu.Unlock()

	logging.Info("session kill requested",
		"session_id", sessionID,
		"tool", sess.Tool,
		"remaining_sessions", remaining,
	)

	// Close the session
	err := sess.Close()

	// Mark as ended in store
	if !m.store.MarkEnded(sessionID) {
		logging.Warn("failed to mark killed session in store",
			"session_id", sessionID,
		)
	}
	if saveErr := m.store.Save(); saveErr != nil {
		logging.Warn("failed to save session store after kill",
			"session_id", sessionID,
			"error", saveErr,
		)
	}

	return err
}

// Count returns the number of active (non-ended) sessions
func (m *Manager) Count() int {
	m.sessionsMu.RLock()
	defer m.sessionsMu.RUnlock()

	count := 0
	for _, sess := range m.sessions {
		if sess.Status() != StatusEnded {
			count++
		}
	}
	return count
}

// GetStore returns the session store for persistence operations
func (m *Manager) GetStore() *Store {
	return m.store
}

// GetLogDir returns the log directory path
func (m *Manager) GetLogDir() string {
	return m.logDir
}

// Close shuts down the manager and all sessions.
// Safe to call multiple times - subsequent calls are no-ops.
func (m *Manager) Close() error {
	// Atomic swap ensures only first caller proceeds
	if m.closed.Swap(true) {
		return nil // Already closed
	}

	logging.Info("session manager shutting down")

	// Stop cleanup loop and wait for it to exit
	// R3 Fix 5: Stop ticker first, then signal done (semantically correct order)
	m.cleanupTicker.Stop()
	close(m.done)
	m.wg.Wait() // Wait for cleanupLoop to exit before acquiring lock

	// Close all sessions
	m.sessionsMu.Lock()

	sessionCount := len(m.sessions)
	sessionIDs := make([]string, 0, sessionCount)
	for id, sess := range m.sessions {
		sess.Close()
		sessionIDs = append(sessionIDs, id)
	}

	m.sessionsMu.Unlock()

	// Mark all sessions as ended in store
	for _, id := range sessionIDs {
		if !m.store.MarkEnded(id) {
			logging.Warn("failed to mark session as ended on shutdown",
				"session_id", id,
			)
		}
	}

	// Save store before shutdown
	if err := m.store.Save(); err != nil {
		logging.Warn("failed to save session store on shutdown", "error", err)
	}

	logging.Info("session manager closed", "sessions_closed", sessionCount)

	return nil
}
