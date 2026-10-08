package session

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gelotto/hqsshd/internal/config"
	"github.com/gelotto/hqsshd/internal/logging"
)

// history_size is legacy: daemon.yaml files written from the old README
// sample (history_size: 10000) must keep the 10 MB default scrollback, not
// shrink to history_size × ~100 bytes. Only an explicit
// max_scrollback_size changes the buffer.
func TestHistorySizeDoesNotShrinkDefaultScrollback(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       int
	}{
		{"legacy history_size only", "sessions:\n  history_size: 10000\n", 10 * 1024 * 1024},
		{"explicit max_scrollback_size", "sessions:\n  history_size: 10000\n  max_scrollback_size: 2097152\n", 2 * 1024 * 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "daemon.yaml")
			if err := os.WriteFile(cfgPath, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadFromPath(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			m := NewManager(cfg.Sessions.IdleTimeout, cfg.Sessions.MaxSessions, cfg.Sessions.HistorySize,
				dir, "", 0, cfg.Sessions.ClientBufferSize, cfg.Sessions.MaxScrollbackSize)
			defer m.Close()
			if m.maxBufferSize != tc.want {
				t.Errorf("maxBufferSize = %d, want %d", m.maxBufferSize, tc.want)
			}
		})
	}
}

// max_sessions is enforced against len(map), which still contains sessions
// whose process has exited until the 1-minute cleanup tick; Count() (what
// GetStatus reports as active_sessions) excludes them.
func TestMaxSessionsCountsLiveSessionsOnly(t *testing.T) {
	m := NewManager(3600, 2, 10000, t.TempDir(), "", 0, 0, 0)
	defer m.Close()

	m.sessionsMu.Lock()
	for _, id := range []string{"s1", "s2"} {
		s := NewSession("p", "shell", "/tmp", "", nil, 80, 24, 1024)
		s.markDone() // process exited, cleanup has not run yet
		m.sessions[id] = s
	}
	m.sessionsMu.Unlock()

	if got := m.Count(); got != 0 {
		t.Fatalf("Count() = %d, want 0 (both sessions ended)", got)
	}
	_, err := m.Create("p", "shell", "/tmp", "", nil, 80, 24)
	if err != nil && strings.Contains(err.Error(), "maximum sessions limit") {
		t.Errorf("Create rejected with %q while Count()==0: ended sessions count toward max_sessions until cleanup", err)
	}
}

// sessions.json is the daemon's own 0600 file, but a null element makes
// Load nil-deref instead of returning an error.
func TestStoreLoadSkipsNullRecords(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, sessionsFile), []byte(`[null]`), 0600); err != nil {
		t.Fatal(err)
	}
	st := NewStore(dir, filepath.Join(dir, "logs"))
	var panicked any
	func() {
		defer func() { panicked = recover() }()
		_ = st.Load()
	}()
	if panicked != nil {
		t.Errorf("Store.Load panicked on [null]: %v", panicked)
	}
}

// "re-attach after restart" (README Data Storage): a running record in
// sessions.json is marked ended by NewManager and is not attachable.
func TestRestartMarksRunningRecordsEndedNoReattach(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir, filepath.Join(dir, "logs"))
	st.Add(&SessionRecord{ID: "abc", Tool: "claude", Status: "running", Created: time.Now()})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	m := NewManager(3600, 20, 10000, dir, "", 0, 0, 0)
	defer m.Close()

	rec := m.GetStore().Get("abc")
	if rec == nil || rec.Status != "ended" {
		t.Fatalf("record after restart = %+v, want status ended", rec)
	}
	if _, _, _, err := m.Attach("abc", 80, 24); err == nil {
		t.Error("Attach succeeded for a pre-restart session")
	}
	t.Logf("confirmed: pre-restart running record -> %q, Attach error: not found (no re-attach after restart)", rec.Status)
}

// Gap marker placement: after a drop, the marker must precede every later
// chunk. Marker and data share one bounded wait; if the consumer frees a
// slot during it, the marker takes it first (the data chunk is dropped if
// no second slot appears in time) so the client never resets its parser
// one chunk too late.
func TestGapMarkerPrecedesLaterChunks(t *testing.T) {
	sess := NewSession("", "shell", "", "t", nil, 80, 24, 0)
	ch := sess.AddClient("c", 2)

	sess.broadcast([]byte("A")) // queue [A]
	sess.broadcast([]byte("B")) // queue [A B] (full)
	sess.broadcast([]byte("C")) // bounded wait expires -> C dropped, gapPending

	done := make(chan struct{})
	go func() {
		sess.broadcast([]byte("D")) // marker does not fit; D waits for a slot
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	recv := func() string {
		select {
		case d := <-ch:
			if len(d) == 0 {
				return "<GAP>"
			}
			return string(d)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for a chunk")
			return ""
		}
	}
	var got []string
	got = append(got, recv()) // A: frees a slot, D is delivered -> queue [B D]
	<-done
	got = append(got, recv())   // B
	got = append(got, recv())   // D (the marker should have preceded it)
	sess.broadcast([]byte("E")) // queue empty: marker fits, then E
	got = append(got, recv())
	got = append(got, recv())

	t.Logf("delivery order: %v (C was dropped)", got)
	if got[2] != "<GAP>" {
		t.Errorf("gap marker delivered at position %d, want right after B (before D): %v", indexOf(got, "<GAP>"), got)
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// Close() on a session whose process already exited (the normal cleanup
// path for every naturally-ended session) escalates SIGTERM->SIGKILL->
// os.Process.Kill on a reaped pid and logs an ERROR.
func TestCloseAfterNaturalExitIsQuiet(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a shell")
	}
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HOME", t.TempDir())

	var buf bytes.Buffer
	old := logging.Logger
	logging.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { logging.Logger = old })

	sess := NewSession("", "shell", "/tmp", "", []string{"true"}, 80, 24, 0)
	if err := sess.StartPTY(); err != nil {
		t.Fatalf("StartPTY: %v", err)
	}
	select {
	case <-sess.Done():
	case <-time.After(15 * time.Second):
		sess.Close()
		t.Fatal("shell did not exit")
	}
	buf.Reset()

	sess.Close()

	logs := buf.String()
	t.Logf("logs during Close() of an exited session:\n%s", logs)
	if strings.Contains(logs, "level=ERROR") && strings.Contains(logs, "failed to kill process") {
		t.Errorf("Close() of an already-exited session logged ERROR 'failed to kill process' (os.Process.Kill on a reaped pid)")
	}
}
