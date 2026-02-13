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
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
)

const (
	defaultCols = 120
	defaultRows = 40
)

// Executor runs tasks and manages their lifecycle
type Executor struct {
	taskStore     *Store
	runStore      *RunStore
	maxOutputSize int64 // max task output in bytes

	// Track running tasks for cancellation
	running   map[string]*runningTask
	runningMu sync.RWMutex
}

type runningTask struct {
	cancel context.CancelFunc
	cmd    *exec.Cmd
}

// NewExecutor creates a new task executor.
// maxOutputSize limits captured task output in bytes (0 = default 1MB).
func NewExecutor(taskStore *Store, runStore *RunStore, maxOutputSize int) *Executor {
	if maxOutputSize <= 0 {
		maxOutputSize = 1024 * 1024 // 1MB default
	}
	return &Executor{
		taskStore:     taskStore,
		runStore:      runStore,
		maxOutputSize: int64(maxOutputSize),
		running:       make(map[string]*runningTask),
	}
}

// Run executes a task and returns the run
func (e *Executor) Run(taskID string, projectPath string) (*Run, error) {
	task := e.taskStore.Get(taskID)
	if task == nil {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}

	// For interactive tasks, we don't support them in MVP
	if task.Interactive {
		return nil, fmt.Errorf("interactive tasks not yet supported")
	}

	// Atomic check-and-create under lock to prevent TOCTOU race
	e.runningMu.Lock()
	if existing := e.runStore.GetRunning(taskID); existing != nil {
		e.runningMu.Unlock()
		return nil, fmt.Errorf("task is already running (run_id: %s)", existing.ID)
	}
	run := e.runStore.Create(taskID)
	e.runningMu.Unlock()

	// Execute in background
	go e.executeTask(task, run, projectPath)

	return run, nil
}

// Cancel cancels a running task
func (e *Executor) Cancel(runID string) error {
	e.runningMu.Lock()
	rt, exists := e.running[runID]
	e.runningMu.Unlock()

	if !exists {
		// Check if the run exists but isn't running
		run := e.runStore.Get(runID)
		if run == nil {
			return fmt.Errorf("run not found: %s", runID)
		}
		if run.Status != RunStatusRunning && run.Status != RunStatusPending {
			return fmt.Errorf("run is not cancellable (status: %d)", run.Status)
		}
		// It's in pending state, just mark it cancelled
		if e.runStore.Cancel(runID) {
			return nil
		}
		return fmt.Errorf("failed to cancel run")
	}

	// Cancel the context and kill the process.
	// Don't call cmd.Wait() here — the executeTask() goroutine handles it.
	// Concurrent Wait() on the same exec.Cmd is undefined behavior.
	rt.cancel()
	if rt.cmd != nil && rt.cmd.Process != nil {
		rt.cmd.Process.Kill()
	}

	e.runStore.Cancel(runID)

	e.runningMu.Lock()
	delete(e.running, runID)
	e.runningMu.Unlock()

	return nil
}

// executeTask runs the task in a PTY and captures output
func (e *Executor) executeTask(task *Task, run *Run, projectPath string) {
	// Mark as running
	e.runStore.UpdateStatus(run.ID, RunStatusRunning)

	// Determine working directory
	workingDir := projectPath
	if workingDir == "" {
		if task.Scope == TaskScopeSystem {
			// Use home directory for system tasks
			homeDir, _ := os.UserHomeDir()
			workingDir = homeDir
		} else {
			// Should have project path for project tasks
			e.runStore.Fail(run.ID, "project path required for project-scoped task")
			return
		}
	}

	// Validate working directory
	if _, err := os.Stat(workingDir); os.IsNotExist(err) {
		e.runStore.Fail(run.ID, fmt.Sprintf("working directory does not exist: %s", workingDir))
		return
	}

	// Setup timeout context
	var ctx context.Context
	var cancel context.CancelFunc
	if task.TimeoutSeconds > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(task.TimeoutSeconds)*time.Second)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	defer cancel()

	// Build command
	cmd := e.buildCommand(task, workingDir)

	// Track for cancellation
	e.runningMu.Lock()
	e.running[run.ID] = &runningTask{
		cancel: cancel,
		cmd:    cmd,
	}
	e.runningMu.Unlock()

	defer func() {
		e.runningMu.Lock()
		delete(e.running, run.ID)
		e.runningMu.Unlock()
	}()

	// Start with PTY for proper terminal handling
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Rows: defaultRows,
		Cols: defaultCols,
	})
	if err != nil {
		e.runStore.Fail(run.ID, fmt.Sprintf("failed to start command: %v", err))
		return
	}
	defer ptmx.Close()

	// Write prompt to stdin (for AI tools that read from stdin)
	if task.Prompt != "" {
		// Send prompt followed by newline
		_, err := ptmx.Write([]byte(task.Prompt + "\n"))
		if err != nil {
			fmt.Printf("Warning: failed to write prompt to task: %v\n", err)
		}

		// For non-interactive AI tools, send EOF after prompt
		// Wait a moment for the tool to read the prompt
		time.Sleep(100 * time.Millisecond)
	}

	// Capture output
	var outputBuf bytes.Buffer
	outputDone := make(chan struct{})

	go func() {
		defer close(outputDone)
		// Read output with size limit
		limitedReader := io.LimitReader(ptmx, e.maxOutputSize)
		n, err := io.Copy(&outputBuf, limitedReader)
		if err != nil {
			fmt.Printf("Warning: error reading task output: %v\n", err)
		}
		// If we hit the size limit, notify the user
		if n >= e.maxOutputSize {
			outputBuf.WriteString(fmt.Sprintf("\n[Output truncated at %dMB]", e.maxOutputSize/(1024*1024)))
		}
	}()

	// Wait for command completion or context cancellation
	cmdDone := make(chan error, 1)
	go func() {
		cmdDone <- cmd.Wait()
	}()

	select {
	case err := <-cmdDone:
		// Command completed
		<-outputDone // Wait for output to be captured

		output := outputBuf.String()
		if err != nil {
			e.runStore.Fail(run.ID, fmt.Sprintf("command failed: %v", err))
			e.runStore.SetOutput(run.ID, output)
		} else {
			e.runStore.Complete(run.ID, output)
		}

	case <-ctx.Done():
		// Timeout or cancelled
		if cmd.Process != nil {
			cmd.Process.Kill()
			// Wait for process to actually exit to avoid orphaned processes
			cmd.Wait()
		}
		<-outputDone // Wait for output to be captured

		output := outputBuf.String()
		e.runStore.SetOutput(run.ID, output)

		if ctx.Err() == context.DeadlineExceeded {
			e.runStore.Fail(run.ID, fmt.Sprintf("task timed out after %d seconds", task.TimeoutSeconds))
		} else {
			e.runStore.Cancel(run.ID)
		}
	}

	// Persist run state after completion
	if err := e.runStore.Save(); err != nil {
		fmt.Printf("Warning: failed to save task run store: %v\n", err)
	}
}

// buildCommand builds the exec.Cmd for the task's tool.
// All non-shell commands are wrapped in a login shell to ensure
// tools installed via nvm/pyenv/asdf are available on PATH.
// Prompts are passed as positional arguments ($1) to prevent shell injection.
func (e *Executor) buildCommand(task *Task, workingDir string) *exec.Cmd {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}

	var cmd *exec.Cmd

	switch task.Tool {
	case "claude":
		// Claude with --print flag for non-interactive mode
		// Prompt passed as $1 to prevent shell expansion
		cmd = exec.Command(shell, "-l", "-c", `claude --print "$1"`, "_", task.Prompt)
	case "codex":
		cmd = exec.Command(shell, "-l", "-c", `codex "$1"`, "_", task.Prompt)
	case "aider":
		// Aider with --yes for non-interactive
		cmd = exec.Command(shell, "-l", "-c", `aider --yes --message "$1"`, "_", task.Prompt)
	case "shell":
		// Intentionally runs prompt as a command — this is the tool's purpose
		cmd = exec.Command(shell, "-l", "-c", task.Prompt)
	default:
		// Tool name already validated against config whitelist
		cmd = exec.Command(shell, "-l", "-c", fmt.Sprintf(`%s "$1"`, task.Tool), "_", task.Prompt)
	}

	cmd.Dir = workingDir
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		fmt.Sprintf("COLUMNS=%d", defaultCols),
		fmt.Sprintf("LINES=%d", defaultRows),
		"CI=true", // Some tools detect CI and act non-interactively
	)

	return cmd
}

// GetRunningCount returns the number of currently running tasks
func (e *Executor) GetRunningCount() int {
	e.runningMu.RLock()
	defer e.runningMu.RUnlock()
	return len(e.running)
}

// Close stops all running tasks.
// Only cancels context and kills processes. The executeTask() goroutine
// handles cmd.Wait() — calling it here too would be a concurrent Wait() race.
func (e *Executor) Close() {
	e.runningMu.Lock()
	// Copy entries to avoid holding the lock during Kill
	snapshot := make(map[string]*runningTask, len(e.running))
	for k, v := range e.running {
		snapshot[k] = v
	}
	e.running = make(map[string]*runningTask)
	e.runningMu.Unlock()

	for runID, rt := range snapshot {
		rt.cancel()
		if rt.cmd != nil && rt.cmd.Process != nil {
			rt.cmd.Process.Kill()
		}
		e.runStore.Cancel(runID)
	}
}
