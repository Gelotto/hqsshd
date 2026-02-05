package session

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewManager(t *testing.T) {
	m := NewManager(3600, 20, 10000, t.TempDir(), "", 0)
	defer m.Close()

	if m == nil {
		t.Fatal("NewManager returned nil")
	}
	if m.idleTimeout != 3600*time.Second {
		t.Errorf("idleTimeout = %v, want %v", m.idleTimeout, 3600*time.Second)
	}
	if m.maxSessions != 20 {
		t.Errorf("maxSessions = %d, want 20", m.maxSessions)
	}
	// 10000 lines * 100 bytes/line = 1MB
	if m.maxBufferSize != 10000*100 {
		t.Errorf("maxBufferSize = %d, want %d", m.maxBufferSize, 10000*100)
	}
}

func TestNewManager_DefaultBufferSize(t *testing.T) {
	m := NewManager(3600, 20, 0, t.TempDir(), "", 0)
	defer m.Close()

	// Should default to 10MB when historySize is 0
	expectedSize := 10 * 1024 * 1024
	if m.maxBufferSize != expectedSize {
		t.Errorf("maxBufferSize = %d, want %d", m.maxBufferSize, expectedSize)
	}
}

func TestManager_GetUnknownSession(t *testing.T) {
	m := NewManager(3600, 20, 10000, t.TempDir(), "", 0)
	defer m.Close()

	sess := m.Get("unknown-session-id")
	if sess != nil {
		t.Errorf("Get(unknown) = %v, want nil", sess)
	}
}

func TestManager_ListEmpty(t *testing.T) {
	m := NewManager(3600, 20, 10000, t.TempDir(), "", 0)
	defer m.Close()

	sessions := m.List("", false)
	if len(sessions) != 0 {
		t.Errorf("List() returned %d sessions, want 0", len(sessions))
	}
}

func TestManager_AttachUnknownSession(t *testing.T) {
	m := NewManager(3600, 20, 10000, t.TempDir(), "", 0)
	defer m.Close()

	_, _, _, err := m.Attach("unknown-session-id", 80, 24)
	if err == nil {
		t.Error("Attach(unknown) should return error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want 'not found'", err)
	}
}

func TestManager_DetachUnknownSession(t *testing.T) {
	m := NewManager(3600, 20, 10000, t.TempDir(), "", 0)
	defer m.Close()

	err := m.Detach("unknown-session-id", "client-1")
	if err == nil {
		t.Error("Detach(unknown) should return error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want 'not found'", err)
	}
}

func TestManager_InputUnknownSession(t *testing.T) {
	m := NewManager(3600, 20, 10000, t.TempDir(), "", 0)
	defer m.Close()

	err := m.Input("unknown-session-id", []byte("test"))
	if err == nil {
		t.Error("Input(unknown) should return error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want 'not found'", err)
	}
}

func TestManager_ResizeUnknownSession(t *testing.T) {
	m := NewManager(3600, 20, 10000, t.TempDir(), "", 0)
	defer m.Close()

	err := m.Resize("unknown-session-id", 80, 24)
	if err == nil {
		t.Error("Resize(unknown) should return error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want 'not found'", err)
	}
}

func TestManager_KillUnknownSession(t *testing.T) {
	m := NewManager(3600, 20, 10000, t.TempDir(), "", 0)
	defer m.Close()

	err := m.Kill("unknown-session-id")
	if err == nil {
		t.Error("Kill(unknown) should return error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want 'not found'", err)
	}
}

func TestManager_CountEmpty(t *testing.T) {
	m := NewManager(3600, 20, 10000, t.TempDir(), "", 0)
	defer m.Close()

	count := m.Count()
	if count != 0 {
		t.Errorf("Count() = %d, want 0", count)
	}
}

func TestManager_CloseIdempotent(t *testing.T) {
	m := NewManager(3600, 20, 10000, t.TempDir(), "", 0)

	// First close should succeed
	err1 := m.Close()
	if err1 != nil {
		t.Errorf("first Close() = %v, want nil", err1)
	}

	// Second close should also succeed (idempotent)
	err2 := m.Close()
	if err2 != nil {
		t.Errorf("second Close() = %v, want nil", err2)
	}
}

func TestManager_CloseConcurrent(t *testing.T) {
	m := NewManager(3600, 20, 10000, t.TempDir(), "", 0)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Close()
		}()
	}
	wg.Wait()
	// Should not panic
}

// TestManager_MaxSessionsLimit tests that the manager rejects new sessions
// when the max limit is reached. This test uses mock sessions to avoid PTY.
func TestManager_MaxSessionsLimit(t *testing.T) {
	m := NewManager(3600, 2, 10000, t.TempDir(), "", 0)
	defer m.Close()

	// Manually add mock sessions to test limit
	m.sessionsMu.Lock()
	m.sessions["sess-1"] = NewSession("proj-1", "shell", "/tmp", nil, 80, 24, 1024)
	m.sessions["sess-2"] = NewSession("proj-2", "shell", "/tmp", nil, 80, 24, 1024)
	m.sessionsMu.Unlock()

	// Now Create should fail (would exceed max of 2)
	_, err := m.Create("proj-3", "shell", "/tmp", nil, 80, 24)
	if err == nil {
		t.Error("Create should fail when max sessions reached")
	}
	if !strings.Contains(err.Error(), "maximum sessions limit") {
		t.Errorf("error = %v, want 'maximum sessions limit'", err)
	}
}

func TestManager_ListByProject(t *testing.T) {
	m := NewManager(3600, 20, 10000, t.TempDir(), "", 0)
	defer m.Close()

	// Add mock sessions
	sess1 := NewSession("proj-a", "claude", "/tmp", nil, 80, 24, 1024)
	sess2 := NewSession("proj-b", "codex", "/tmp", nil, 80, 24, 1024)
	sess3 := NewSession("proj-a", "aider", "/tmp", nil, 80, 24, 1024)

	m.sessionsMu.Lock()
	m.sessions[sess1.ID] = sess1
	m.sessions[sess2.ID] = sess2
	m.sessions[sess3.ID] = sess3
	m.sessionsMu.Unlock()

	// List by project
	projASessions := m.List("proj-a", false)
	if len(projASessions) != 2 {
		t.Errorf("List(proj-a) = %d sessions, want 2", len(projASessions))
	}

	projBSessions := m.List("proj-b", false)
	if len(projBSessions) != 1 {
		t.Errorf("List(proj-b) = %d sessions, want 1", len(projBSessions))
	}

	// List all
	allSessions := m.List("", false)
	if len(allSessions) != 3 {
		t.Errorf("List('') = %d sessions, want 3", len(allSessions))
	}
}

func TestManager_ListExcludesEnded(t *testing.T) {
	m := NewManager(3600, 20, 10000, t.TempDir(), "", 0)
	defer m.Close()

	// Add mock sessions
	activeSess := NewSession("proj-a", "claude", "/tmp", nil, 80, 24, 1024)
	endedSess := NewSession("proj-a", "codex", "/tmp", nil, 80, 24, 1024)
	endedSess.markDone() // Mark as ended

	m.sessionsMu.Lock()
	m.sessions[activeSess.ID] = activeSess
	m.sessions[endedSess.ID] = endedSess
	m.sessionsMu.Unlock()

	// Without includeEnded
	sessions := m.List("", false)
	if len(sessions) != 1 {
		t.Errorf("List(includeEnded=false) = %d, want 1", len(sessions))
	}

	// With includeEnded
	sessionsWithEnded := m.List("", true)
	if len(sessionsWithEnded) != 2 {
		t.Errorf("List(includeEnded=true) = %d, want 2", len(sessionsWithEnded))
	}
}

func TestManager_CountExcludesEnded(t *testing.T) {
	m := NewManager(3600, 20, 10000, t.TempDir(), "", 0)
	defer m.Close()

	// Add mock sessions
	activeSess := NewSession("proj-a", "claude", "/tmp", nil, 80, 24, 1024)
	endedSess := NewSession("proj-a", "codex", "/tmp", nil, 80, 24, 1024)
	endedSess.markDone()

	m.sessionsMu.Lock()
	m.sessions[activeSess.ID] = activeSess
	m.sessions[endedSess.ID] = endedSess
	m.sessionsMu.Unlock()

	count := m.Count()
	if count != 1 {
		t.Errorf("Count() = %d, want 1 (excluding ended)", count)
	}
}
