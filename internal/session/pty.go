package session

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"
	"github.com/gelotto/hqsshd/internal/logging"
)

// StartPTY starts the PTY process for the session
func (s *Session) StartPTY() error {
	// Build the command
	cmd, err := s.buildCommand()
	if err != nil {
		logging.Error("failed to build command",
			"session_id", s.ID,
			"tool", s.Tool,
			"error", err,
		)
		return fmt.Errorf("failed to build command: %w", err)
	}

	// Get dimensions (thread-safe)
	cols, rows := s.GetDimensions()

	// Set terminal size
	size := &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	}

	// Start the command with a PTY
	ptmx, err := pty.StartWithSize(cmd, size)
	if err != nil {
		logging.Error("failed to start PTY",
			"session_id", s.ID,
			"tool", s.Tool,
			"error", err,
		)
		return fmt.Errorf("failed to start pty: %w", err)
	}

	s.pty = ptmx
	s.cmd = cmd.Process

	logging.Info("session PTY started",
		"session_id", s.ID,
		"tool", s.Tool,
		"pid", cmd.Process.Pid,
		"working_dir", s.WorkingDirectory,
	)

	// Start reading PTY output
	go s.readPTYOutput()

	return nil
}

// buildCommand builds the exec.Cmd for the session's tool
func (s *Session) buildCommand() (*exec.Cmd, error) {
	var cmd *exec.Cmd

	switch s.Tool {
	case "claude":
		cmd = exec.Command("claude", s.Args...)
	case "codex":
		cmd = exec.Command("codex", s.Args...)
	case "aider":
		cmd = exec.Command("aider", s.Args...)
	case "shell":
		// Default to user's shell or bash
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/bash"
		}
		// For shell, args are passed as -c "command" if provided
		if len(s.Args) > 0 {
			cmd = exec.Command(shell, append([]string{"-c"}, s.Args...)...)
		} else {
			cmd = exec.Command(shell)
		}
	default:
		// Try to run the tool as a command with any args
		cmd = exec.Command(s.Tool, s.Args...)
	}

	// Set working directory
	cmd.Dir = s.WorkingDirectory

	// Get dimensions (thread-safe)
	cols, rows := s.GetDimensions()

	// Set environment
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		fmt.Sprintf("COLUMNS=%d", cols),
		fmt.Sprintf("LINES=%d", rows),
	)

	return cmd, nil
}

// readPTYOutput continuously reads from PTY and broadcasts to clients
func (s *Session) readPTYOutput() {
	defer close(s.readDone) // Signal that this goroutine has exited

	logging.Debug("PTY reader started", "session_id", s.ID)

	buf := make([]byte, 4096)

	for {
		n, err := s.pty.Read(buf)
		if err != nil {
			if err == io.EOF {
				// Process exited normally
				logging.Info("PTY process exited",
					"session_id", s.ID,
					"reason", "eof",
				)
				s.markDone()
				return
			}
			// Other error - log and exit
			logging.Warn("PTY read error",
				"session_id", s.ID,
				"error", err,
			)
			s.markDone()
			return
		}

		if n > 0 {
			// Make a copy of the data
			data := make([]byte, n)
			copy(data, buf[:n])

			// Update activity
			s.UpdateActivity()

			// Add to scrollback buffer
			s.appendToBuffer(data)

			// Broadcast to all clients
			s.broadcast(data)
		}
	}
}

// Write sends input to the PTY
func (s *Session) Write(data []byte) (int, error) {
	if s.pty == nil {
		return 0, fmt.Errorf("session PTY not initialized")
	}

	if s.IsDone() {
		return 0, fmt.Errorf("session has ended")
	}

	s.UpdateActivity()
	return s.pty.Write(data)
}

// Resize changes the PTY terminal size
func (s *Session) Resize(cols, rows int) error {
	if s.pty == nil {
		return fmt.Errorf("session PTY not initialized")
	}

	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("invalid terminal size: cols=%d, rows=%d (must be positive)", cols, rows)
	}

	s.ptySizeMu.Lock()
	defer s.ptySizeMu.Unlock()

	s.Cols = cols
	s.Rows = rows

	size := &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	}

	return pty.Setsize(s.pty, size)
}

// Kill terminates the session's process
func (s *Session) Kill() error {
	if s.cmd == nil {
		return nil
	}

	logging.Info("killing session process",
		"session_id", s.ID,
		"pid", s.cmd.Pid,
	)

	// Send SIGTERM first
	if err := s.cmd.Signal(os.Interrupt); err != nil {
		// If interrupt fails, try kill
		logging.Debug("SIGINT failed, sending SIGKILL",
			"session_id", s.ID,
			"error", err,
		)
		if killErr := s.cmd.Kill(); killErr != nil {
			logging.Error("failed to kill process",
				"session_id", s.ID,
				"error", killErr,
			)
			return fmt.Errorf("failed to kill process: %w", killErr)
		}
	}

	return nil
}

// Close cleans up session resources.
// Uses sync.Once to ensure idempotent behavior - safe to call multiple times.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		logging.Info("closing session",
			"session_id", s.ID,
			"tool", s.Tool,
		)

		// Mark as done
		s.markDone()

		// Kill process first to initiate shutdown
		if s.cmd != nil {
			s.Kill()
		}

		// Close PTY to unblock readPTYOutput (it's blocked on Read)
		// MUST happen before waiting for goroutine!
		if s.pty != nil {
			s.pty.Close()
		}

		// NOW wait for readPTYOutput goroutine to exit
		// The PTY close above will cause Read() to return an error
		select {
		case <-s.readDone:
			// Goroutine exited cleanly
			logging.Debug("PTY reader exited cleanly", "session_id", s.ID)
		case <-time.After(2 * time.Second):
			// Timeout - proceed with cleanup anyway
			logging.Warn("PTY reader timeout",
				"session_id", s.ID,
				"timeout", "2s",
			)
		}

		// Close all client channels (safe now that broadcast stopped)
		s.clientsMu.Lock()
		clientCount := len(s.clients)
		for id, ch := range s.clients {
			close(ch)
			delete(s.clients, id)
		}
		s.clientsMu.Unlock()

		logging.Info("session closed",
			"session_id", s.ID,
			"disconnected_clients", clientCount,
		)
	})

	return nil
}
