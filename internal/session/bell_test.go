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

func TestBellDetector(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   []bool // expected result per chunk
	}{
		{
			name:   "bare bell",
			chunks: []string{"done\x07"},
			want:   []bool{true},
		},
		{
			name:   "no bell",
			chunks: []string{"compiling...\r\n"},
			want:   []bool{false},
		},
		{
			name:   "OSC title terminated by BEL is ignored",
			chunks: []string{"\x1b]0;claude — running task\x07"},
			want:   []bool{false},
		},
		{
			name:   "bell after OSC sequence ends",
			chunks: []string{"\x1b]0;title\x07ready\x07"},
			want:   []bool{true},
		},
		{
			name:   "OSC terminated by ST then bell",
			chunks: []string{"\x1b]0;title\x1b\\", "\x07"},
			want:   []bool{false, true},
		},
		{
			name:   "OSC split across chunks",
			chunks: []string{"\x1b]0;long ti", "tle here\x07", "\x07"},
			want:   []bool{false, false, true},
		},
		{
			name:   "ESC split across chunk boundary",
			chunks: []string{"\x1b", "]0;t\x07"},
			want:   []bool{false, false},
		},
		{
			name:   "CSI sequences do not affect detection",
			chunks: []string{"\x1b[2J\x1b[31mred\x1b[0m\x07"},
			want:   []bool{true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &bellDetector{}
			for i, chunk := range tt.chunks {
				got := d.process([]byte(chunk))
				if got != tt.want[i] {
					t.Errorf("chunk %d: process(%q) = %v, want %v",
						i, chunk, got, tt.want[i])
				}
			}
		})
	}
}

func TestEventHub(t *testing.T) {
	hub := NewEventHub()

	id1, ch1 := hub.Subscribe()
	id2, ch2 := hub.Subscribe()
	if hub.SubscriberCount() != 2 {
		t.Fatalf("SubscriberCount() = %d, want 2", hub.SubscriberCount())
	}

	event := Event{
		SessionID:   "test-session",
		SessionName: "myapp/claude-abcd",
		Tool:        "claude",
		Type:        EventTypeBell,
		Timestamp:   time.Now(),
	}
	hub.Publish(event)

	for i, ch := range []<-chan Event{ch1, ch2} {
		select {
		case got := <-ch:
			if got.SessionID != event.SessionID || got.Type != EventTypeBell {
				t.Errorf("subscriber %d got %+v, want %+v", i, got, event)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d did not receive event", i)
		}
	}

	// Unsubscribe closes the channel and stops delivery
	hub.Unsubscribe(id1)
	if hub.SubscriberCount() != 1 {
		t.Fatalf("SubscriberCount() = %d, want 1", hub.SubscriberCount())
	}
	if _, open := <-ch1; open {
		t.Error("expected ch1 to be closed after Unsubscribe")
	}

	// Publishing to a full buffer must not block
	for i := 0; i < 100; i++ {
		hub.Publish(event)
	}

	hub.Unsubscribe(id2)
	if hub.SubscriberCount() != 0 {
		t.Fatalf("SubscriberCount() = %d, want 0", hub.SubscriberCount())
	}
}

func TestSessionEmitsEndedEvent(t *testing.T) {
	hub := NewEventHub()
	_, ch := hub.Subscribe()

	sess := NewSession("", "shell", "", "test", nil, 80, 24, 0)
	sess.SetEventCallback(hub.Publish)
	sess.markDone()

	select {
	case got := <-ch:
		if got.Type != EventTypeEnded || got.SessionID != sess.ID {
			t.Errorf("got %+v, want ended event for %s", got, sess.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive ended event")
	}
}
