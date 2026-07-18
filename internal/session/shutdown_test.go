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
	"testing"
	"time"
)

func TestEventHubCloseEndsSubscribers(t *testing.T) {
	hub := NewEventHub()
	_, ch := hub.Subscribe()

	hub.Close()

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected closed channel, got an event")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber channel not closed by hub Close")
	}
}

func TestEventHubSubscribeAfterClose(t *testing.T) {
	hub := NewEventHub()
	hub.Close()

	_, ch := hub.Subscribe()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected closed channel from post-close Subscribe")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("post-close Subscribe returned a channel that never closes")
	}
}

func TestEventHubPublishAfterCloseIsNoOp(t *testing.T) {
	hub := NewEventHub()
	hub.Close()

	// Must not panic (send on closed channel) or record history
	hub.Publish(Event{SessionID: "s1", Type: EventTypeBell, Timestamp: time.Now()})
	if got := len(hub.Recent(0)); got != 0 {
		t.Errorf("Recent after post-close Publish = %d events, want 0", got)
	}
}

func TestEventHubCloseIsIdempotent(t *testing.T) {
	hub := NewEventHub()
	hub.Subscribe()
	hub.Close()
	hub.Close() // must not double-close channels
}

func TestManagerCreateAfterCloseRejected(t *testing.T) {
	m := NewManager(3600, 20, 10000, t.TempDir(), "", 0, 0, 0)
	if err := m.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if _, err := m.Create("proj", "shell", "", "", nil, 80, 24); err == nil {
		t.Error("Create after Close succeeded, want error")
	}
}
