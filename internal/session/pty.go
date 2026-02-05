package session

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
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
	s.execCmd = cmd

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
// All commands are wrapped in a login+interactive shell (-l -i) to ensure the user's full
// environment (PATH, etc.) is available. The -i flag is critical because .bashrc typically
// has an early-exit guard for non-interactive shells (case $- in *i*) ...).
func (s *Session) buildCommand() (*exec.Cmd, error) {
	// Get user's login shell
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}

	var cmd *exec.Cmd

	switch s.Tool {
	case "shell":
		// Interactive shell session - use login shell directly
		if len(s.Args) > 0 {
			// Run a command in the shell
			cmd = exec.Command(shell, "-l", "-i", "-c", strings.Join(s.Args, " "))
		} else {
			// Interactive login shell
			cmd = exec.Command(shell, "-l")
		}
	default:
		// Tools (claude, codex, aider, etc.) - wrap in login shell
		// This ensures PATH from .bashrc/.bash_profile is loaded
		toolCmd := s.Tool
		if len(s.Args) > 0 {
			// Quote args that contain spaces for shell execution
			quotedArgs := make([]string, len(s.Args))
			for i, arg := range s.Args {
				if strings.ContainsAny(arg, " \t\"'") {
					quotedArgs[i] = fmt.Sprintf("%q", arg)
				} else {
					quotedArgs[i] = arg
				}
			}
			toolCmd = s.Tool + " " + strings.Join(quotedArgs, " ")
		}
		cmd = exec.Command(shell, "-l", "-i", "-c", toolCmd)

		logging.Debug("building command with login shell",
			"session_id", s.ID,
			"shell", shell,
			"tool_cmd", toolCmd,
		)
	}

	// Set working directory
	cmd.Dir = s.WorkingDirectory

	// Get dimensions (thread-safe)
	cols, rows := s.GetDimensions()

	// Set environment - the login shell will source user's config files
	// and override/extend these with the user's PATH, etc.
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
			// Reap the child process to avoid zombies
			if s.execCmd != nil {
				s.execCmd.Wait()
			}

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

			// Add to scrollback buffer (in-memory, for fast attach)
			s.appendToBuffer(data)

			// Persist to disk (if logger is set)
			if s.logger != nil {
				if _, err := s.logger.Write(data); err != nil {
					logging.Warn("failed to write to session log",
						"session_id", s.ID,
						"error", err,
					)
				}
			}

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

	// Send SIGTERM for graceful shutdown
	if err := s.cmd.Signal(syscall.SIGTERM); err != nil {
		// If SIGTERM fails, try SIGKILL
		logging.Debug("SIGTERM failed, sending SIGKILL",
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

		// Close the logger (triggers compression)
		if s.logger != nil {
			if err := s.logger.Close(); err != nil {
				logging.Warn("failed to close session logger",
					"session_id", s.ID,
					"error", err,
				)
			}
		}

		logging.Info("session closed",
			"session_id", s.ID,
			"disconnected_clients", clientCount,
		)
	})

	return nil
}
