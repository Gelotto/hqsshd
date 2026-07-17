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

// Package session provides PTY session management for AI tools
package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/gelotto/hqsshd/internal/logging"
	"github.com/google/uuid"
)

// Status represents the current state of a session
type Status int

const (
	StatusUnspecified Status = iota
	StatusRunning            // Session is active and process is running
	StatusIdle               // No clients attached but process still running
	StatusEnded              // Process has terminated
)

func (s Status) String() string {
	switch s {
	case StatusRunning:
		return "running"
	case StatusIdle:
		return "idle"
	case StatusEnded:
		return "ended"
	default:
		return "unspecified"
	}
}

// clientState tracks a connected client's output channel and drop statistics
type clientState struct {
	ch            chan []byte
	dropCount     int64
	lastDropTime  time.Time
}

// Session represents a persistent PTY session running an AI tool
type Session struct {
	ID               string
	Name             string   // Human-readable name (e.g., "myapp/claude-a1b2")
	ProjectID        string
	Tool             string   // 'claude', 'codex', 'aider', 'shell'
	Args             []string // Additional arguments for the tool
	WorkingDirectory string
	CreatedAt        time.Time

	// Terminal dimensions
	Cols int
	Rows int

	// Protected fields (use getters/setters)
	status       Status
	lastActivity time.Time
	statusMu     sync.RWMutex

	// PTY management
	pty       *os.File
	cmd       *os.Process
	execCmd   *exec.Cmd // Retained for cmd.Wait() to reap the child process
	ptySizeMu sync.Mutex

	// Client management
	clients   map[string]*clientState // clientID -> client state
	clientsMu sync.RWMutex

	// Output buffer for scrollback (clients get history on attach)
	outputBuffer   []byte
	outputBufferMu sync.Mutex
	maxBufferSize  int

	// Persistent logging (writes to disk for history preservation)
	logger *SessionLogger

	// Event emission (set by Manager via SetEventCallback; may be nil).
	// bell and lastBellEvent are only touched by the PTY reader goroutine.
	onEvent       func(Event)
	bell          bellDetector
	lastBellEvent time.Time

	// Terminal mode state for attach replay (internally synchronized)
	modes modeTracker

	// Lifecycle management
	done      chan struct{}
	doneOnce  sync.Once
	closeOnce sync.Once     // R3 Fix 1: Ensure Close() is idempotent
	readDone  chan struct{} // Signals readPTYOutput goroutine has exited
}

// NewSession creates a new session with the given parameters.
// If name is empty, auto-generates from working directory, tool, and short ID.
func NewSession(projectID, tool, workingDir, name string, args []string, cols, rows, maxBufferSize int) *Session {
	if maxBufferSize <= 0 {
		maxBufferSize = 1024 * 1024 // 1MB default
	}
	now := time.Now()
	id := uuid.New().String()

	// Auto-generate name if not provided
	if name == "" {
		shortID := id[:4]
		if workingDir != "" {
			// Use last path component as repo name
			dirName := filepath.Base(workingDir)
			name = dirName + "/" + tool + "-" + shortID
		} else {
			name = tool + "-" + shortID
		}
	}

	sess := &Session{
		ID:               id,
		Name:             name,
		ProjectID:        projectID,
		Tool:             tool,
		Args:             args,
		WorkingDirectory: workingDir,
		status:           StatusIdle, // Start as Idle until client attaches
		CreatedAt:        now,
		lastActivity:     now,
		Cols:             cols,
		Rows:             rows,
		clients:          make(map[string]*clientState),
		outputBuffer:     make([]byte, 0, 64*1024), // 64KB initial capacity
		maxBufferSize:    maxBufferSize,
		done:             make(chan struct{}),
		readDone:         make(chan struct{}),
	}

	logging.Info("session created",
		"session_id", sess.ID,
		"name", name,
		"project_id", projectID,
		"tool", tool,
		"working_dir", workingDir,
		"cols", cols,
		"rows", rows,
	)

	return sess
}

// Status returns the current session status (thread-safe)
func (s *Session) Status() Status {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	return s.status
}

// LastActivity returns the last activity timestamp (thread-safe)
func (s *Session) LastActivity() time.Time {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	return s.lastActivity
}

// ClientCount returns the number of attached clients
func (s *Session) ClientCount() int {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()
	return len(s.clients)
}

// GetDimensions returns the terminal dimensions (thread-safe)
func (s *Session) GetDimensions() (cols, rows int) {
	s.ptySizeMu.Lock()
	defer s.ptySizeMu.Unlock()
	return s.Cols, s.Rows
}

// UpdateActivity updates the last activity timestamp (thread-safe)
func (s *Session) UpdateActivity() {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.lastActivity = time.Now()
}

// IsDone returns true if the session has ended
func (s *Session) IsDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// Done returns a channel that closes when the session ends
func (s *Session) Done() <-chan struct{} {
	return s.done
}

// SetLogger sets the session logger for persistent output storage
func (s *Session) SetLogger(logger *SessionLogger) {
	s.logger = logger
}

// SetEventCallback sets the callback invoked for session events (bell, end).
// Must be called before StartPTY.
func (s *Session) SetEventCallback(fn func(Event)) {
	s.onEvent = fn
}

// emitEvent invokes the event callback with a snapshot of session identity
func (s *Session) emitEvent(t EventType) {
	if s.onEvent == nil {
		return
	}
	s.onEvent(Event{
		SessionID:   s.ID,
		SessionName: s.Name,
		Tool:        s.Tool,
		Type:        t,
		Timestamp:   time.Now(),
	})
}

// GetLogger returns the session logger (may be nil)
func (s *Session) GetLogger() *SessionLogger {
	return s.logger
}

// markDone marks the session as ended (called internally)
func (s *Session) markDone() {
	s.doneOnce.Do(func() {
		s.statusMu.Lock()
		oldStatus := s.status
		s.status = StatusEnded
		s.statusMu.Unlock()

		logging.Info("session ended",
			"session_id", s.ID,
			"from_status", oldStatus.String(),
			"tool", s.Tool,
			"project_id", s.ProjectID,
		)

		close(s.done)
		s.emitEvent(EventTypeEnded)
	})
}

// AddClient adds a new client and returns its output channel.
// bufferSize controls the output channel capacity.
func (s *Session) AddClient(clientID string, bufferSize int) <-chan []byte {
	if bufferSize <= 0 {
		bufferSize = 256
	}
	// Create buffered channel for output
	cs := &clientState{
		ch: make(chan []byte, bufferSize),
	}

	// Track if we need to update status (avoid nested locks)
	var shouldUpdateStatus bool
	var clientCount int

	s.clientsMu.Lock()
	s.clients[clientID] = cs
	clientCount = len(s.clients)
	shouldUpdateStatus = clientCount == 1
	s.clientsMu.Unlock()

	// Update status AFTER releasing clientsMu (prevents deadlock)
	if shouldUpdateStatus {
		s.statusMu.Lock()
		oldStatus := s.status
		if s.status == StatusIdle {
			s.status = StatusRunning
		}
		s.statusMu.Unlock()

		if oldStatus == StatusIdle {
			logging.Info("session status changed",
				"session_id", s.ID,
				"from", oldStatus.String(),
				"to", StatusRunning.String(),
				"reason", "first_client_attached",
			)
		}
	}

	logging.Debug("client attached",
		"session_id", s.ID,
		"client_id", clientID,
		"client_count", clientCount,
	)

	return cs.ch
}

// RemoveClient removes a client from the session
func (s *Session) RemoveClient(clientID string) {
	// Track if we need to update status (avoid nested locks)
	var shouldUpdateStatus bool
	var clientCount int
	var found bool

	var dropCount int64

	s.clientsMu.Lock()
	if cs, ok := s.clients[clientID]; ok {
		found = true
		dropCount = cs.dropCount
		close(cs.ch)
		delete(s.clients, clientID)
	}
	clientCount = len(s.clients)
	shouldUpdateStatus = clientCount == 0
	s.clientsMu.Unlock()

	if found {
		logFn := logging.Debug
		logArgs := []any{
			"session_id", s.ID,
			"client_id", clientID,
			"client_count", clientCount,
		}
		if dropCount > 0 {
			logFn = logging.Warn
			logArgs = append(logArgs, "total_drops", dropCount)
		}
		logFn("client detached", logArgs...)
	}

	// Update status AFTER releasing clientsMu (prevents deadlock)
	if shouldUpdateStatus {
		s.statusMu.Lock()
		oldStatus := s.status
		if s.status == StatusRunning {
			s.status = StatusIdle
		}
		s.statusMu.Unlock()

		if oldStatus == StatusRunning {
			logging.Info("session status changed",
				"session_id", s.ID,
				"from", oldStatus.String(),
				"to", StatusIdle.String(),
				"reason", "last_client_detached",
			)
		}
	}
}

// ModePreamble returns escape sequences restoring the session's current DEC
// private mode state (alt buffer, mouse tracking, bracketed paste, ...) for
// clients attaching after those sequences were trimmed from the scrollback.
func (s *Session) ModePreamble() []byte {
	return s.modes.preamble()
}

// GetScrollback returns the output buffer for client catch-up
func (s *Session) GetScrollback() []byte {
	s.outputBufferMu.Lock()
	defer s.outputBufferMu.Unlock()

	// Return a copy to avoid races
	result := make([]byte, len(s.outputBuffer))
	copy(result, s.outputBuffer)
	return result
}

// appendToBuffer adds data to the scrollback buffer
func (s *Session) appendToBuffer(data []byte) {
	s.outputBufferMu.Lock()
	defer s.outputBufferMu.Unlock()

	s.outputBuffer = append(s.outputBuffer, data...)

	// Trim if over max size (keep the most recent maxBufferSize bytes)
	if len(s.outputBuffer) > s.maxBufferSize {
		s.outputBuffer = s.outputBuffer[len(s.outputBuffer)-s.maxBufferSize:]
	}
}

// broadcast sends data to all attached clients
func (s *Session) broadcast(data []byte) {
	// Check if session has ended to prevent send on closed channels
	// (RemoveClient closes channels when session ends)
	if s.IsDone() {
		return
	}

	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	for clientID, cs := range s.clients {
		// Non-blocking send - drop if channel is full.
		// Recover from panic in case channel was closed between
		// the IsDone() check above and this send (narrow race with Close()).
		func() {
			defer func() { recover() }()
			select {
			case cs.ch <- data:
			default:
				cs.dropCount++
				cs.lastDropTime = time.Now()
				// Log warning at powers of 100 (100, 1000, 10000, ...)
				if cs.dropCount == 100 || (cs.dropCount > 0 && cs.dropCount%1000 == 0) {
					logging.Warn("client output drops",
						"session_id", s.ID,
						"client_id", clientID,
						"total_drops", cs.dropCount,
					)
				}
			}
		}()
	}
}

// SessionInfo is a serializable snapshot of session state (for persistence/API)
type SessionInfo struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	ProjectID        string    `json:"project_id"`
	Tool             string    `json:"tool"`
	WorkingDirectory string    `json:"working_directory"`
	Status           Status    `json:"status"`
	CreatedAt        time.Time `json:"created_at"`
	LastActivity     time.Time `json:"last_activity"`
	Cols             int       `json:"cols"`
	Rows             int       `json:"rows"`
	ClientCount      int       `json:"client_count"`
}

// Info returns a serializable snapshot of the session
func (s *Session) Info() SessionInfo {
	cols, rows := s.GetDimensions()
	return SessionInfo{
		ID:               s.ID,
		Name:             s.Name,
		ProjectID:        s.ProjectID,
		Tool:             s.Tool,
		WorkingDirectory: s.WorkingDirectory,
		Status:           s.Status(),
		CreatedAt:        s.CreatedAt,
		LastActivity:     s.LastActivity(),
		Cols:             cols,
		Rows:             rows,
		ClientCount:      s.ClientCount(),
	}
}
