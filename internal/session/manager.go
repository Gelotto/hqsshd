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
	idleTimeout   time.Duration
	maxSessions   int
	maxBufferSize int // bytes for scrollback buffer
	cleanupTicker *time.Ticker
	done          chan struct{}
	wg            sync.WaitGroup // Wait for cleanupLoop to exit
	closed        atomic.Bool    // Prevent double-close panic
}

// NewManager creates a new session manager
// historySize is in lines - we estimate ~100 bytes per line for buffer sizing
func NewManager(idleTimeoutSec, maxSessions, historySize int) *Manager {
	// Convert lines to bytes (estimate ~100 bytes per line)
	maxBufferSize := historySize * 100
	if maxBufferSize <= 0 {
		maxBufferSize = 1024 * 1024 // 1MB default
	}

	m := &Manager{
		sessions:      make(map[string]*Session),
		idleTimeout:   time.Duration(idleTimeoutSec) * time.Second,
		maxSessions:   maxSessions,
		maxBufferSize: maxBufferSize,
		done:          make(chan struct{}),
	}

	// Start cleanup routine
	m.cleanupTicker = time.NewTicker(1 * time.Minute)
	m.wg.Add(1)
	go m.cleanupLoop()

	return m
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
	m.sessionsMu.Lock()
	defer m.sessionsMu.Unlock()

	now := time.Now()
	toDelete := []string{}
	var endedCount, idleTimeoutCount int

	for id, sess := range m.sessions {
		status := sess.Status()

		// Remove ended sessions
		if status == StatusEnded {
			toDelete = append(toDelete, id)
			endedCount++
			continue
		}

		// Check idle timeout
		if m.idleTimeout > 0 && status == StatusIdle {
			idleFor := now.Sub(sess.LastActivity())
			if idleFor > m.idleTimeout {
				logging.Info("session idle timeout",
					"session_id", id,
					"tool", sess.Tool,
					"idle_duration", idleFor.String(),
					"idle_timeout", m.idleTimeout.String(),
				)
				sess.Close()
				toDelete = append(toDelete, id)
				idleTimeoutCount++
			}
		}
	}

	for _, id := range toDelete {
		delete(m.sessions, id)
	}

	// Log cleanup summary if any sessions were removed
	if len(toDelete) > 0 {
		logging.Debug("cleanup completed",
			"removed_ended", endedCount,
			"removed_idle_timeout", idleTimeoutCount,
			"remaining_sessions", len(m.sessions),
		)
	}
}

// Create creates a new session with the given parameters
func (m *Manager) Create(projectID, tool, workingDir string, args []string, cols, rows int) (*Session, error) {
	m.sessionsMu.Lock()
	defer m.sessionsMu.Unlock()

	// Check max sessions limit
	if m.maxSessions > 0 && len(m.sessions) >= m.maxSessions {
		logging.Warn("max sessions limit reached",
			"max_sessions", m.maxSessions,
			"current_sessions", len(m.sessions),
		)
		return nil, fmt.Errorf("maximum sessions limit (%d) reached", m.maxSessions)
	}

	// Create session with config-based buffer size
	sess := NewSession(projectID, tool, workingDir, args, cols, rows, m.maxBufferSize)

	// Start PTY
	if err := sess.StartPTY(); err != nil {
		return nil, fmt.Errorf("failed to start PTY: %w", err)
	}

	// Store session
	m.sessions[sess.ID] = sess

	logging.Info("session registered",
		"session_id", sess.ID,
		"tool", tool,
		"project_id", projectID,
		"total_sessions", len(m.sessions),
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

	// Get scrollback before attaching (for catch-up)
	scrollback := sess.GetScrollback()

	// Add client
	outputCh := sess.AddClient(clientID)

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

	return sess.Close()
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
	defer m.sessionsMu.Unlock()

	sessionCount := len(m.sessions)
	for _, sess := range m.sessions {
		sess.Close()
	}

	logging.Info("session manager closed", "sessions_closed", sessionCount)

	return nil
}
