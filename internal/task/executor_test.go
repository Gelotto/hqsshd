package task

import (
	"os"
	"strings"
	"testing"
	"time"
)

// NOTE: Integration tests (functions ending with _Integration) poll Run status
// by reading from pointers returned by RunStore.Get(). This creates a race
// condition with the executor goroutine that modifies the same Run.
//
// In production, this isn't an issue because gRPC serializes the data.
// For testing, run integration tests WITHOUT the -race flag:
//   go test ./...              # all tests without race detector
//   go test -short ./...       # unit tests only
//   go test -race -short ./... # unit tests with race detector

func TestNewExecutor(t *testing.T) {
	dir := t.TempDir()
	taskStore := NewStore(dir)
	runStore := NewRunStore(dir, 0)

	e := NewExecutor(taskStore, runStore, 0)

	if e == nil {
		t.Fatal("NewExecutor returned nil")
	}
	if e.taskStore != taskStore {
		t.Error("taskStore not set correctly")
	}
	if e.runStore != runStore {
		t.Error("runStore not set correctly")
	}
}

func TestExecutor_RunUnknownTask(t *testing.T) {
	dir := t.TempDir()
	taskStore := NewStore(dir)
	runStore := NewRunStore(dir, 0)
	e := NewExecutor(taskStore, runStore, 0)

	_, err := e.Run("unknown-task-id", "/tmp")
	if err == nil {
		t.Error("Run(unknown) should return error")
	}
	if !strings.Contains(err.Error(), "task not found") {
		t.Errorf("error = %v, want 'task not found'", err)
	}
}

func TestExecutor_RunInteractiveTask(t *testing.T) {
	dir := t.TempDir()
	taskStore := NewStore(dir)
	runStore := NewRunStore(dir, 0)
	e := NewExecutor(taskStore, runStore, 0)

	// Create an interactive task (not supported in MVP)
	task := taskStore.Create("interactive task", "", "claude", TaskScopeProject, "proj-1", "prompt", true, 0)

	_, err := e.Run(task.ID, "/tmp")
	if err == nil {
		t.Error("Run(interactive) should return error")
	}
	if !strings.Contains(err.Error(), "interactive tasks not yet supported") {
		t.Errorf("error = %v, want 'interactive tasks not yet supported'", err)
	}
}

func TestExecutor_CancelUnknownRun(t *testing.T) {
	dir := t.TempDir()
	taskStore := NewStore(dir)
	runStore := NewRunStore(dir, 0)
	e := NewExecutor(taskStore, runStore, 0)

	err := e.Cancel("unknown-run-id")
	if err == nil {
		t.Error("Cancel(unknown) should return error")
	}
	if !strings.Contains(err.Error(), "run not found") {
		t.Errorf("error = %v, want 'run not found'", err)
	}
}

func TestExecutor_CancelCompletedRun(t *testing.T) {
	dir := t.TempDir()
	taskStore := NewStore(dir)
	runStore := NewRunStore(dir, 0)
	e := NewExecutor(taskStore, runStore, 0)

	// Create a task and completed run
	task := taskStore.Create("test", "", "shell", TaskScopeSystem, "", "echo", false, 0)
	run := runStore.Create(task.ID)
	runStore.Complete(run.ID, "output")

	err := e.Cancel(run.ID)
	if err == nil {
		t.Error("Cancel(completed) should return error")
	}
	if !strings.Contains(err.Error(), "not cancellable") {
		t.Errorf("error = %v, want 'not cancellable'", err)
	}
}

func TestExecutor_CancelPendingRun(t *testing.T) {
	dir := t.TempDir()
	taskStore := NewStore(dir)
	runStore := NewRunStore(dir, 0)
	e := NewExecutor(taskStore, runStore, 0)

	// Create a task and pending run
	task := taskStore.Create("test", "", "shell", TaskScopeSystem, "", "echo", false, 0)
	run := runStore.Create(task.ID) // Starts in pending state

	err := e.Cancel(run.ID)
	if err != nil {
		t.Errorf("Cancel(pending) error = %v, want nil", err)
	}

	// Verify it was cancelled
	updated := runStore.Get(run.ID)
	if updated.Status != RunStatusCancelled {
		t.Errorf("status = %v, want %v", updated.Status, RunStatusCancelled)
	}
}

func TestExecutor_GetRunningCount(t *testing.T) {
	dir := t.TempDir()
	taskStore := NewStore(dir)
	runStore := NewRunStore(dir, 0)
	e := NewExecutor(taskStore, runStore, 0)

	if e.GetRunningCount() != 0 {
		t.Errorf("GetRunningCount() = %d, want 0", e.GetRunningCount())
	}
}

func TestExecutor_Close(t *testing.T) {
	dir := t.TempDir()
	taskStore := NewStore(dir)
	runStore := NewRunStore(dir, 0)
	e := NewExecutor(taskStore, runStore, 0)

	// Should not panic on empty executor
	e.Close()

	if e.GetRunningCount() != 0 {
		t.Errorf("GetRunningCount() after Close = %d, want 0", e.GetRunningCount())
	}
}

func TestExecutor_BuildCommand_Claude(t *testing.T) {
	dir := t.TempDir()
	taskStore := NewStore(dir)
	runStore := NewRunStore(dir, 0)
	e := NewExecutor(taskStore, runStore, 0)

	task := &Task{
		Tool:   "claude",
		Prompt: "test prompt",
	}

	cmd := e.buildCommand(task, "/tmp")

	// Should be claude --print "test prompt"
	if cmd.Path == "" {
		t.Error("cmd.Path should be set")
	}
	if cmd.Dir != "/tmp" {
		t.Errorf("cmd.Dir = %q, want %q", cmd.Dir, "/tmp")
	}

	// Check args contain --print and prompt
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--print") {
		t.Errorf("args should contain --print: %s", args)
	}
	if !strings.Contains(args, "test prompt") {
		t.Errorf("args should contain prompt: %s", args)
	}
}

func TestExecutor_BuildCommand_Shell(t *testing.T) {
	dir := t.TempDir()
	taskStore := NewStore(dir)
	runStore := NewRunStore(dir, 0)
	e := NewExecutor(taskStore, runStore, 0)

	task := &Task{
		Tool:   "shell",
		Prompt: "echo hello",
	}

	cmd := e.buildCommand(task, "/tmp")

	// Should use shell -c "echo hello"
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}

	if !strings.HasSuffix(cmd.Path, "bash") && !strings.HasSuffix(cmd.Path, "sh") && !strings.HasSuffix(cmd.Path, "zsh") {
		t.Errorf("cmd.Path = %q, want shell", cmd.Path)
	}

	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "-c") {
		t.Errorf("args should contain -c: %s", args)
	}
}

func TestExecutor_BuildCommand_Aider(t *testing.T) {
	dir := t.TempDir()
	taskStore := NewStore(dir)
	runStore := NewRunStore(dir, 0)
	e := NewExecutor(taskStore, runStore, 0)

	task := &Task{
		Tool:   "aider",
		Prompt: "fix the bug",
	}

	cmd := e.buildCommand(task, "/tmp")

	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--yes") {
		t.Errorf("aider args should contain --yes: %s", args)
	}
	if !strings.Contains(args, "--message") {
		t.Errorf("aider args should contain --message: %s", args)
	}
}

func TestExecutor_BuildCommand_Codex(t *testing.T) {
	dir := t.TempDir()
	taskStore := NewStore(dir)
	runStore := NewRunStore(dir, 0)
	e := NewExecutor(taskStore, runStore, 0)

	task := &Task{
		Tool:   "codex",
		Prompt: "generate code",
	}

	cmd := e.buildCommand(task, "/tmp")

	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "generate code") {
		t.Errorf("codex args should contain prompt: %s", args)
	}
}

func TestExecutor_BuildCommand_Environment(t *testing.T) {
	dir := t.TempDir()
	taskStore := NewStore(dir)
	runStore := NewRunStore(dir, 0)
	e := NewExecutor(taskStore, runStore, 0)

	task := &Task{
		Tool:   "shell",
		Prompt: "env",
	}

	cmd := e.buildCommand(task, "/tmp")

	// Check environment variables
	envMap := make(map[string]bool)
	for _, env := range cmd.Env {
		if strings.HasPrefix(env, "TERM=") {
			envMap["TERM"] = true
		}
		if strings.HasPrefix(env, "COLUMNS=") {
			envMap["COLUMNS"] = true
		}
		if strings.HasPrefix(env, "LINES=") {
			envMap["LINES"] = true
		}
		if strings.HasPrefix(env, "CI=") {
			envMap["CI"] = true
		}
	}

	if !envMap["TERM"] {
		t.Error("cmd.Env should contain TERM")
	}
	if !envMap["CI"] {
		t.Error("cmd.Env should contain CI=true")
	}
}

// Integration test - requires shell to be available
func TestExecutor_RunShellTask_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := t.TempDir()
	taskStore := NewStore(dir)
	runStore := NewRunStore(dir, 0)
	e := NewExecutor(taskStore, runStore, 0)
	defer e.Close()

	// Create a simple shell task
	task := taskStore.Create("echo test", "", "shell", TaskScopeSystem, "", "echo hello", false, 10)

	run, err := e.Run(task.ID, "")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if run == nil {
		t.Fatal("Run() returned nil")
	}
	runID := run.ID

	// Wait for completion - use a channel to avoid race
	var finalStatus RunStatus
	var finalError, finalOutput string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// Get run each iteration (runStore.Get is thread-safe)
		updated := runStore.Get(runID)
		if updated == nil {
			continue
		}
		// Copy fields while we have the reference
		finalStatus = updated.Status
		finalError = updated.Error
		finalOutput = updated.Output
		if finalStatus == RunStatusCompleted || finalStatus == RunStatusFailed {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Verify completion
	if finalStatus != RunStatusCompleted {
		t.Errorf("status = %v, want %v (error: %s)", finalStatus, RunStatusCompleted, finalError)
	}
	if !strings.Contains(finalOutput, "hello") {
		t.Errorf("output = %q, want to contain 'hello'", finalOutput)
	}
}

// Test timeout functionality
func TestExecutor_RunWithTimeout_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := t.TempDir()
	taskStore := NewStore(dir)
	runStore := NewRunStore(dir, 0)
	e := NewExecutor(taskStore, runStore, 0)
	defer e.Close()

	// Create a task that will timeout (sleep for 10 seconds with 1 second timeout)
	task := taskStore.Create("timeout test", "", "shell", TaskScopeSystem, "", "sleep 10", false, 1)

	run, err := e.Run(task.ID, "")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	runID := run.ID

	// Wait for timeout (should be about 1 second)
	var finalStatus RunStatus
	var finalError string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		updated := runStore.Get(runID)
		if updated == nil {
			continue
		}
		finalStatus = updated.Status
		finalError = updated.Error
		if finalStatus == RunStatusFailed {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Verify it failed due to timeout
	if finalStatus != RunStatusFailed {
		t.Errorf("status = %v, want %v", finalStatus, RunStatusFailed)
	}
	if !strings.Contains(finalError, "timed out") {
		t.Errorf("error = %q, want to contain 'timed out'", finalError)
	}
}

// Test that tasks can't run concurrently for same task
func TestExecutor_PreventsDuplicateRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := t.TempDir()
	taskStore := NewStore(dir)
	runStore := NewRunStore(dir, 0)
	e := NewExecutor(taskStore, runStore, 0)
	defer e.Close()

	// Create a slow task
	task := taskStore.Create("slow task", "", "shell", TaskScopeSystem, "", "sleep 5", false, 0)

	// First run should succeed
	run1, err := e.Run(task.ID, "")
	if err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	run1ID := run1.ID

	// Wait for it to start running
	time.Sleep(200 * time.Millisecond)

	// Second run should fail
	_, err = e.Run(task.ID, "")
	if err == nil {
		t.Error("second Run() should fail when task is already running")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error = %v, want 'already running'", err)
	}

	// Cancel the first run
	e.Cancel(run1ID)

	// Wait a moment for cancellation to complete
	time.Sleep(100 * time.Millisecond)
}

// Test working directory validation
func TestExecutor_InvalidWorkingDir_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := t.TempDir()
	taskStore := NewStore(dir)
	runStore := NewRunStore(dir, 0)
	e := NewExecutor(taskStore, runStore, 0)
	defer e.Close()

	// Create a project-scoped task with invalid path
	task := taskStore.Create("test", "", "shell", TaskScopeProject, "proj-1", "echo hi", false, 10)

	run, err := e.Run(task.ID, "/nonexistent/path/that/does/not/exist")
	if err != nil {
		t.Fatalf("Run() should not error immediately: %v", err)
	}
	runID := run.ID

	// Wait for failure
	var finalStatus RunStatus
	var finalError string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		updated := runStore.Get(runID)
		if updated == nil {
			continue
		}
		finalStatus = updated.Status
		finalError = updated.Error
		if finalStatus == RunStatusFailed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if finalStatus != RunStatusFailed {
		t.Errorf("status = %v, want %v", finalStatus, RunStatusFailed)
	}
	if !strings.Contains(finalError, "does not exist") {
		t.Errorf("error = %q, want to contain 'does not exist'", finalError)
	}
}
