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
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gelotto/hqsshd/internal/config"
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
	// Validate tool name (defense-in-depth; server.go also validates)
	if s.Tool != "shell" && !config.ValidateToolName(s.Tool) {
		return nil, fmt.Errorf("invalid tool name: %q", s.Tool)
	}

	// Get and validate user's login shell
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	// Verify the shell binary exists
	if _, err := os.Stat(shell); err != nil {
		logging.Warn("configured SHELL not found, falling back to /bin/sh",
			"shell", shell,
			"error", err,
		)
		shell = "/bin/sh"
	}

	var cmd *exec.Cmd

	switch s.Tool {
	case "shell":
		// Interactive shell session - use login shell directly
		if len(s.Args) > 0 {
			// Pass args as positional parameters to prevent injection
			shellArgs := append([]string{"-l", "-i", "-c", `"$@"`, "_"}, s.Args...)
			cmd = exec.Command(shell, shellArgs...)
		} else {
			// Interactive login shell
			cmd = exec.Command(shell, "-l")
		}
	default:
		// Tools (claude, codex, aider, etc.) - wrap in login shell
		// This ensures PATH from .bashrc/.bash_profile is loaded
		// Args passed via positional parameters to prevent shell injection
		if len(s.Args) > 0 {
			// Pass tool + args via positional parameters ("$@" expands safely)
			allArgs := append([]string{s.Tool}, s.Args...)
			shellArgs := append([]string{"-l", "-i", "-c", `"$@"`, "_"}, allArgs...)
			cmd = exec.Command(shell, shellArgs...)
		} else {
			cmd = exec.Command(shell, "-l", "-i", "-c", s.Tool)
		}

		logging.Debug("building command with login shell",
			"session_id", s.ID,
			"shell", shell,
			"tool", s.Tool,
			"args_count", len(s.Args),
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

			// Track DEC private mode state (alt buffer, mouse tracking) so
			// attach replay can restore it after the buffer trims the
			// original sequences
			s.modes.process(data)

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

			// Emit attention event on bare terminal bell (AI CLIs ring it
			// when finished or awaiting input), rate-limited per session
			if s.bell.process(data) && time.Since(s.lastBellEvent) >= bellEventCooldown {
				s.lastBellEvent = time.Now()
				s.emitEvent(EventTypeBell)
			}
		}
	}
}

// bellEventCooldown limits how often a session emits bell events.
const bellEventCooldown = 5 * time.Second

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

// Kill terminates the session's process and its entire process group.
func (s *Session) Kill() error {
	if s.cmd == nil {
		return nil
	}

	logging.Info("killing session process group",
		"session_id", s.ID,
		"pid", s.cmd.Pid,
	)

	// Send SIGTERM to the entire process group for graceful shutdown.
	// Negative PID targets the process group (setsid makes PID = PGID).
	if err := syscall.Kill(-s.cmd.Pid, syscall.SIGTERM); err != nil {
		// If SIGTERM fails, try SIGKILL on the process group
		logging.Debug("SIGTERM to process group failed, sending SIGKILL",
			"session_id", s.ID,
			"error", err,
		)
		if killErr := syscall.Kill(-s.cmd.Pid, syscall.SIGKILL); killErr != nil {
			// Final fallback: kill just the main process
			if fallbackErr := s.cmd.Kill(); fallbackErr != nil {
				logging.Error("failed to kill process",
					"session_id", s.ID,
					"error", fallbackErr,
				)
				return fmt.Errorf("failed to kill process: %w", fallbackErr)
			}
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
		for id, cs := range s.clients {
			close(cs.ch)
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
