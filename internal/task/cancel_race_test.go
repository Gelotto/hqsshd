package task

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// blockingProbe stands in for the interactive-shell PATH probe (up to 5 s
// in production): it signals entered, then blocks until ctx is cancelled.
func blockingProbe(entered chan<- struct{}) func(ctx context.Context) (string, bool) {
	return func(ctx context.Context) (string, bool) {
		entered <- struct{}{}
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
		return "", false
	}
}

// A run cancelled while executeTask is still probing PATH used to be
// unregistered: Cancel marked it CANCELLED and returned success, then the
// task started anyway and Complete overwrote the status.
func TestCancelDuringPathProbeNeverStarts(t *testing.T) {
	dir := t.TempDir()
	ts := NewStore(dir)
	rs := NewRunStore(dir, 0)
	e := NewExecutor(ts, rs, 0, 0)
	defer e.Close()
	entered := make(chan struct{}, 1)
	e.pathProbe = blockingProbe(entered)

	marker := filepath.Join(dir, "started")
	task := ts.Create("touch", "", "shell", TaskScopeSystem, "", "touch "+marker, false, 30)
	run, err := e.Run(task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := e.Cancel(run.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	e.wg.Wait()

	if got := rs.Get(run.ID); got.Status != RunStatusCancelled {
		t.Errorf("status = %v (err %q), want CANCELLED", got.Status, got.Error)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the task ran although Cancel returned success before it started")
	}
}

// Same window during daemon shutdown: Close must stop a run that is still
// probing, and wait for it to record its state.
func TestCloseDuringPathProbeNeverStarts(t *testing.T) {
	dir := t.TempDir()
	ts := NewStore(dir)
	rs := NewRunStore(dir, 0)
	e := NewExecutor(ts, rs, 0, 0)
	entered := make(chan struct{}, 1)
	e.pathProbe = blockingProbe(entered)

	marker := filepath.Join(dir, "started")
	task := ts.Create("touch", "", "shell", TaskScopeSystem, "", "touch "+marker, false, 30)
	run, err := e.Run(task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	e.Close() // waits for executeTask

	if got := rs.Get(run.ID); got.Status != RunStatusCancelled {
		t.Errorf("status = %v (err %q), want CANCELLED", got.Status, got.Error)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the task ran after Close")
	}
	if _, err := e.Run(task.ID, ""); err == nil || !strings.Contains(err.Error(), "shut down") {
		t.Errorf("Run after Close = %v, want a shut-down error", err)
	}
}

// Two quick Run calls: the first run is still PENDING (probing), and the
// duplicate check only looked at RUNNING runs, so both started.
func TestRunRefusesDuplicateWhilePending(t *testing.T) {
	dir := t.TempDir()
	ts := NewStore(dir)
	rs := NewRunStore(dir, 0)
	e := NewExecutor(ts, rs, 0, 0)
	defer e.Close()
	entered := make(chan struct{}, 2)
	e.pathProbe = blockingProbe(entered)

	task := ts.Create("echo", "", "shell", TaskScopeSystem, "", "true", false, 30)
	first, err := e.Run(task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(task.ID, ""); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("second Run = %v, want already running (first run %s pending)", err, first.ID)
	}
}

// Complete/Fail arriving after a cancel must not overwrite CANCELLED.
func TestCompleteAndFailDoNotOverrideCancelled(t *testing.T) {
	rs := NewRunStore(t.TempDir(), 0)
	run := rs.Create("t")
	if !rs.Cancel(run.ID) {
		t.Fatal("Cancel(pending) = false")
	}
	if rs.MarkRunning(run.ID) {
		t.Error("MarkRunning on a cancelled run = true")
	}
	rs.Complete(run.ID, "out")
	rs.Fail(run.ID, "boom")
	if got := rs.Get(run.ID); got.Status != RunStatusCancelled {
		t.Errorf("status = %v, want CANCELLED", got.Status)
	}
}

// Runs persisted as pending/running cannot still be executing after a
// restart; left as-is they would block their task forever.
func TestLoadFailsStaleActiveRuns(t *testing.T) {
	dir := t.TempDir()
	rs := NewRunStore(dir, 0)
	run := rs.Create("t")
	rs.MarkRunning(run.ID)
	if err := rs.Save(); err != nil {
		t.Fatal(err)
	}

	loaded := NewRunStore(dir, 0)
	if err := loaded.Load(); err != nil {
		t.Fatal(err)
	}
	if got := loaded.Get(run.ID); got.Status != RunStatusFailed {
		t.Errorf("status after reload = %v, want FAILED", got.Status)
	}
	if got := loaded.GetRunning("t"); got != nil {
		t.Errorf("GetRunning after reload = %s, want none", got.ID)
	}
}
