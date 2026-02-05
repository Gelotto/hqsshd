// Package session provides PTY session management for AI tools
package session

import (
	"os"
	"os/exec"
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

// Session represents a persistent PTY session running an AI tool
type Session struct {
	ID               string
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
	clients   map[string]chan []byte // clientID -> output channel
	clientsMu sync.RWMutex

	// Output buffer for scrollback (clients get history on attach)
	outputBuffer   []byte
	outputBufferMu sync.Mutex
	maxBufferSize  int

	// Persistent logging (writes to disk for history preservation)
	logger *SessionLogger

	// Lifecycle management
	done      chan struct{}
	doneOnce  sync.Once
	closeOnce sync.Once     // R3 Fix 1: Ensure Close() is idempotent
	readDone  chan struct{} // Signals readPTYOutput goroutine has exited
}

// NewSession creates a new session with the given parameters
func NewSession(projectID, tool, workingDir string, args []string, cols, rows, maxBufferSize int) *Session {
	if maxBufferSize <= 0 {
		maxBufferSize = 1024 * 1024 // 1MB default
	}
	now := time.Now()
	sess := &Session{
		ID:               uuid.New().String(),
		ProjectID:        projectID,
		Tool:             tool,
		Args:             args,
		WorkingDirectory: workingDir,
		status:           StatusIdle, // Start as Idle until client attaches
		CreatedAt:        now,
		lastActivity:     now,
		Cols:             cols,
		Rows:             rows,
		clients:          make(map[string]chan []byte),
		outputBuffer:     make([]byte, 0, 64*1024), // 64KB initial capacity
		maxBufferSize:    maxBufferSize,
		done:             make(chan struct{}),
		readDone:         make(chan struct{}),
	}

	logging.Info("session created",
		"session_id", sess.ID,
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
	})
}

// AddClient adds a new client and returns its output channel
func (s *Session) AddClient(clientID string) <-chan []byte {
	// Create buffered channel for output
	ch := make(chan []byte, 256)

	// Track if we need to update status (avoid nested locks)
	var shouldUpdateStatus bool
	var clientCount int

	s.clientsMu.Lock()
	s.clients[clientID] = ch
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

	return ch
}

// RemoveClient removes a client from the session
func (s *Session) RemoveClient(clientID string) {
	// Track if we need to update status (avoid nested locks)
	var shouldUpdateStatus bool
	var clientCount int
	var found bool

	s.clientsMu.Lock()
	if ch, ok := s.clients[clientID]; ok {
		found = true
		close(ch)
		delete(s.clients, clientID)
	}
	clientCount = len(s.clients)
	shouldUpdateStatus = clientCount == 0
	s.clientsMu.Unlock()

	if found {
		logging.Debug("client detached",
			"session_id", s.ID,
			"client_id", clientID,
			"client_count", clientCount,
		)
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

	for _, ch := range s.clients {
		// Non-blocking send - drop if channel is full.
		// Recover from panic in case channel was closed between
		// the IsDone() check above and this send (narrow race with Close()).
		func() {
			defer func() { recover() }()
			select {
			case ch <- data:
			default:
			}
		}()
	}
}

// SessionInfo is a serializable snapshot of session state (for persistence/API)
type SessionInfo struct {
	ID               string    `json:"id"`
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
