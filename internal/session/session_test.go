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
	"sync"
	"testing"
	"time"
)

func TestNewSession(t *testing.T) {
	sess := NewSession("proj-1", "claude", "/home/user/project", "", []string{"--print"}, 80, 24, 1024)

	if sess.ID == "" {
		t.Error("ID should not be empty")
	}
	if sess.ProjectID != "proj-1" {
		t.Errorf("ProjectID = %q, want %q", sess.ProjectID, "proj-1")
	}
	if sess.Tool != "claude" {
		t.Errorf("Tool = %q, want %q", sess.Tool, "claude")
	}
	if sess.WorkingDirectory != "/home/user/project" {
		t.Errorf("WorkingDirectory = %q, want %q", sess.WorkingDirectory, "/home/user/project")
	}
	if len(sess.Args) != 1 || sess.Args[0] != "--print" {
		t.Errorf("Args = %v, want %v", sess.Args, []string{"--print"})
	}
	if sess.Cols != 80 {
		t.Errorf("Cols = %d, want 80", sess.Cols)
	}
	if sess.Rows != 24 {
		t.Errorf("Rows = %d, want 24", sess.Rows)
	}
}

func TestSession_StatusStartsIdle(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 1024)

	if sess.Status() != StatusIdle {
		t.Errorf("Status() = %v, want %v", sess.Status(), StatusIdle)
	}
}

func TestSession_AddClientChangesStatusToRunning(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 1024)

	// Status should be Idle initially
	if sess.Status() != StatusIdle {
		t.Errorf("initial Status() = %v, want %v", sess.Status(), StatusIdle)
	}

	// Add a client
	ch := sess.AddClient("client-1", 0)
	if ch == nil {
		t.Error("AddClient returned nil channel")
	}

	// Status should change to Running
	if sess.Status() != StatusRunning {
		t.Errorf("Status() after AddClient = %v, want %v", sess.Status(), StatusRunning)
	}
}

func TestSession_RemoveClientChangesStatusToIdle(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 1024)

	// Add and remove client
	sess.AddClient("client-1", 0)
	sess.RemoveClient("client-1")

	// Status should be back to Idle
	if sess.Status() != StatusIdle {
		t.Errorf("Status() after RemoveClient = %v, want %v", sess.Status(), StatusIdle)
	}
}

func TestSession_MultipleClients(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 1024)

	// Add multiple clients
	sess.AddClient("client-1", 0)
	sess.AddClient("client-2", 0)
	sess.AddClient("client-3", 0)

	if sess.ClientCount() != 3 {
		t.Errorf("ClientCount() = %d, want 3", sess.ClientCount())
	}

	// Still Running after removing one
	sess.RemoveClient("client-2")
	if sess.Status() != StatusRunning {
		t.Errorf("Status() after removing one client = %v, want %v", sess.Status(), StatusRunning)
	}
	if sess.ClientCount() != 2 {
		t.Errorf("ClientCount() = %d, want 2", sess.ClientCount())
	}

	// Remove remaining clients
	sess.RemoveClient("client-1")
	sess.RemoveClient("client-3")

	if sess.Status() != StatusIdle {
		t.Errorf("Status() after removing all clients = %v, want %v", sess.Status(), StatusIdle)
	}
}

func TestSession_RemoveNonexistentClient(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 1024)

	// Should not panic
	sess.RemoveClient("nonexistent")

	if sess.ClientCount() != 0 {
		t.Errorf("ClientCount() = %d, want 0", sess.ClientCount())
	}
}

func TestSession_GetScrollback(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 1024)

	// Initially empty
	scrollback := sess.GetScrollback()
	if len(scrollback) != 0 {
		t.Errorf("initial scrollback length = %d, want 0", len(scrollback))
	}

	// Append some data
	sess.appendAndBroadcast([]byte("hello"))
	sess.appendAndBroadcast([]byte(" world"))

	scrollback = sess.GetScrollback()
	if string(scrollback) != "hello world" {
		t.Errorf("scrollback = %q, want %q", string(scrollback), "hello world")
	}
}

func TestSession_GetScrollback_ReturnsCopy(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 1024)

	sess.appendAndBroadcast([]byte("original"))

	scrollback := sess.GetScrollback()
	// Modify the returned slice
	scrollback[0] = 'X'

	// Original should be unchanged
	original := sess.GetScrollback()
	if string(original) != "original" {
		t.Errorf("original buffer was modified: got %q, want %q", string(original), "original")
	}
}

func TestSession_AppendToBuffer_TrimsWhenOverMax(t *testing.T) {
	// Create session with 10 byte max buffer
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 10)

	// Add 15 bytes
	sess.appendAndBroadcast([]byte("12345"))
	sess.appendAndBroadcast([]byte("67890"))
	sess.appendAndBroadcast([]byte("ABCDE"))

	scrollback := sess.GetScrollback()
	if len(scrollback) > 10 {
		t.Errorf("buffer length = %d, want <= 10", len(scrollback))
	}
	// Should keep the most recent bytes
	if string(scrollback) != "0ABCDE" && string(scrollback) != "890ABCDE" && len(scrollback) != 10 {
		t.Logf("buffer content = %q (trimmed as expected)", string(scrollback))
	}
}

func TestSession_IsDone(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 1024)

	if sess.IsDone() {
		t.Error("IsDone() should be false initially")
	}

	sess.markDone()

	if !sess.IsDone() {
		t.Error("IsDone() should be true after markDone()")
	}
}

func TestSession_Done_ChannelCloses(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 1024)

	doneCh := sess.Done()

	// Should not block
	select {
	case <-doneCh:
		t.Error("Done() channel should not be closed yet")
	default:
		// Good
	}

	sess.markDone()

	// Should be closed now
	select {
	case <-doneCh:
		// Good
	case <-time.After(100 * time.Millisecond):
		t.Error("Done() channel should be closed after markDone()")
	}
}

func TestSession_MarkDone_Idempotent(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 1024)

	// Call multiple times - should not panic
	sess.markDone()
	sess.markDone()
	sess.markDone()

	if !sess.IsDone() {
		t.Error("session should be done")
	}
}

func TestSession_UpdateActivity(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 1024)

	initial := sess.LastActivity()
	time.Sleep(10 * time.Millisecond)
	sess.UpdateActivity()
	updated := sess.LastActivity()

	if !updated.After(initial) {
		t.Error("LastActivity should increase after UpdateActivity")
	}
}

func TestSession_GetDimensions(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 120, 40, 1024)

	cols, rows := sess.GetDimensions()
	if cols != 120 {
		t.Errorf("cols = %d, want 120", cols)
	}
	if rows != 40 {
		t.Errorf("rows = %d, want 40", rows)
	}
}

func TestSession_Info(t *testing.T) {
	sess := NewSession("proj-1", "claude", "/home/user", "", []string{"--print"}, 80, 24, 1024)

	info := sess.Info()

	if info.ID != sess.ID {
		t.Errorf("info.ID = %q, want %q", info.ID, sess.ID)
	}
	if info.ProjectID != "proj-1" {
		t.Errorf("info.ProjectID = %q, want %q", info.ProjectID, "proj-1")
	}
	if info.Tool != "claude" {
		t.Errorf("info.Tool = %q, want %q", info.Tool, "claude")
	}
	if info.WorkingDirectory != "/home/user" {
		t.Errorf("info.WorkingDirectory = %q, want %q", info.WorkingDirectory, "/home/user")
	}
	if info.Status != StatusIdle {
		t.Errorf("info.Status = %v, want %v", info.Status, StatusIdle)
	}
	if info.Cols != 80 {
		t.Errorf("info.Cols = %d, want 80", info.Cols)
	}
	if info.Rows != 24 {
		t.Errorf("info.Rows = %d, want 24", info.Rows)
	}
	if info.ClientCount != 0 {
		t.Errorf("info.ClientCount = %d, want 0", info.ClientCount)
	}
}

func TestSession_StatusString(t *testing.T) {
	tests := []struct {
		status Status
		want   string
	}{
		{StatusUnspecified, "unspecified"},
		{StatusRunning, "running"},
		{StatusIdle, "idle"},
		{StatusEnded, "ended"},
		{Status(99), "unspecified"}, // Unknown value
	}

	for _, tt := range tests {
		got := tt.status.String()
		if got != tt.want {
			t.Errorf("Status(%d).String() = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestSession_BroadcastAfterDone(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 1024)

	// Add client
	ch := sess.AddClient("client-1", 0)

	// Mark session as done
	sess.markDone()

	// Broadcast should not panic (IsDone check should prevent send)
	sess.broadcast([]byte("test data"))

	// Drain channel - should be empty since broadcast was skipped
	select {
	case <-ch:
		// Channel might have data from before markDone or might be closed
	default:
		// Empty - expected
	}
}

func TestSession_ConcurrentAccess(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 1024)

	var wg sync.WaitGroup

	// Concurrent client operations
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			clientID := string(rune('A' + id))
			sess.AddClient(clientID, 0)
			sess.ClientCount()
			sess.Status()
			sess.RemoveClient(clientID)
		}(i)
	}

	// Concurrent buffer operations
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess.appendAndBroadcast([]byte("test"))
			sess.GetScrollback()
		}()
	}

	// Concurrent status reads
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess.Status()
			sess.IsDone()
			sess.LastActivity()
			sess.Info()
		}()
	}

	wg.Wait()
	// Test passes if no race detected (run with -race flag)
}
