package task

// Regression tests from the September 2026 review.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitRun(t *testing.T, rs *RunStore, id string, timeout time.Duration) *Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r := rs.Get(id)
		if r != nil && (r.Status == RunStatusCompleted || r.Status == RunStatusFailed || r.Status == RunStatusCancelled) {
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
	return rs.Get(id)
}

// The prompt is passed as $1 AND written to the task's PTY stdin
// (executor.go "Write prompt to stdin"). A shell task that reads stdin
// therefore consumes its own command text.
func TestPromptNotWrittenToPty(t *testing.T) {
	dir := t.TempDir()
	ts := NewStore(dir)
	rs := NewRunStore(dir, 0)
	e := NewExecutor(ts, rs, 0, 0)
	defer e.Close()

	task := ts.Create("stdin", "", "shell", TaskScopeSystem, "", "head -n1 | tr a-z A-Z", false, 10)
	run, err := e.Run(task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	final := waitRun(t, rs, run.ID, 8*time.Second)
	t.Logf("status=%v err=%q output=%q", final.Status, final.Error, final.Output)
	if strings.Contains(final.Output, "HEAD -N1") {
		t.Errorf("the task read its own prompt from stdin: output=%q", final.Output)
	}
}

// Output beyond max_output_size is never drained: the child blocks on a
// full PTY and the run only ends at the timeout.
func TestOutputOverLimitIsDrained(t *testing.T) {
	dir := t.TempDir()
	ts := NewStore(dir)
	rs := NewRunStore(dir, 0)
	e := NewExecutor(ts, rs, 4096, 0) // 4 KB cap
	defer e.Close()

	// ~300 KB of output then a marker; should complete in well under a second
	task := ts.Create("big", "", "shell", TaskScopeSystem, "", "head -c 300000 /dev/zero | tr '\\0' x; echo; echo DONE", false, 4)
	start := time.Now()
	run, err := e.Run(task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	final := waitRun(t, rs, run.ID, 10*time.Second)
	elapsed := time.Since(start)
	t.Logf("status=%v err=%q elapsed=%v outputLen=%d", final.Status, final.Error, elapsed, len(final.Output))
	if final.Status != RunStatusCompleted {
		t.Errorf("run did not complete: status=%v err=%q after %v (expected COMPLETED with truncated output)", final.Status, final.Error, elapsed)
	}
}

// Shell metacharacters in the prompt are not expanded for non-shell tools
// (the "$1" positional-argument claim), verified with a fake tool on PATH.
func TestPromptNotShellExpandedForTools(t *testing.T) {
	bin := t.TempDir()
	script := filepath.Join(bin, "hqreviewtool")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'ARGS:%s\\n' \"$*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	ts := NewStore(dir)
	rs := NewRunStore(dir, 0)
	e := NewExecutor(ts, rs, 0, 0)
	defer e.Close()

	prompt := "$(echo INJECTED) ; echo INJECTED2 `id`"
	task := ts.Create("inj", "", "hqreviewtool", TaskScopeSystem, "", prompt, false, 10)
	run, err := e.Run(task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	final := waitRun(t, rs, run.ID, 8*time.Second)
	t.Logf("status=%v err=%q output=%q", final.Status, final.Error, final.Output)
	if final.Status != RunStatusCompleted {
		t.Fatalf("run failed: %v %q", final.Status, final.Error)
	}
	if !strings.Contains(final.Output, "ARGS:$(echo INJECTED) ; echo INJECTED2 `id`") {
		t.Errorf("prompt was not passed verbatim as $1: %q", final.Output)
	}
	if strings.Contains(final.Output, "ARGS:INJECTED") {
		t.Errorf("prompt was shell-expanded: %q", final.Output)
	}
}

// Cancel() marks the run CANCELLED and SIGKILLs the process group;
// executeTask's select then races cmdDone (-> Fail "signal: killed")
// against ctx.Done() (-> stays CANCELLED).
func TestCancelKeepsCancelledStatus(t *testing.T) {
	dir := t.TempDir()
	ts := NewStore(dir)
	rs := NewRunStore(dir, 0)
	e := NewExecutor(ts, rs, 0, 0)
	defer e.Close()

	failed := 0
	const iterations = 15
	for i := 0; i < iterations; i++ {
		task := ts.Create("sleep", "", "shell", TaskScopeSystem, "", "sleep 5", false, 30)
		run, err := e.Run(task.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		// wait until the process is registered as running
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && e.GetRunningCount() == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		if err := e.Cancel(run.ID); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		final := waitRun(t, rs, run.ID, 5*time.Second)
		if final.Status != RunStatusCancelled {
			failed++
			t.Logf("iteration %d: status=%v err=%q", i, final.Status, final.Error)
		}
		// let executeTask finish before the next iteration
		deadline = time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && e.GetRunningCount() != 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if failed > 0 {
		t.Errorf("%d/%d cancelled runs ended with a non-CANCELLED status", failed, iterations)
	}
}
