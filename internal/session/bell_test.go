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
	"strings"
	"testing"
	"time"
)

func TestBellDetector(t *testing.T) {
	tests := []struct {
		name     string
		chunks   []string
		wantBell []bool     // expected bare-bell result per chunk
		wantMsgs [][]string // expected OSC 9 messages per chunk (nil = none)
	}{
		{
			name:     "bare bell",
			chunks:   []string{"done\x07"},
			wantBell: []bool{true},
			wantMsgs: [][]string{nil},
		},
		{
			name:     "no bell",
			chunks:   []string{"compiling...\r\n"},
			wantBell: []bool{false},
			wantMsgs: [][]string{nil},
		},
		{
			name:     "OSC title terminated by BEL is ignored",
			chunks:   []string{"\x1b]0;claude — running task\x07"},
			wantBell: []bool{false},
			wantMsgs: [][]string{nil},
		},
		{
			name:     "bell after OSC sequence ends",
			chunks:   []string{"\x1b]0;title\x07ready\x07"},
			wantBell: []bool{true},
			wantMsgs: [][]string{nil},
		},
		{
			name:     "OSC terminated by ST then bell",
			chunks:   []string{"\x1b]0;title\x1b\\", "\x07"},
			wantBell: []bool{false, true},
			wantMsgs: [][]string{nil, nil},
		},
		{
			name:     "OSC split across chunks",
			chunks:   []string{"\x1b]0;long ti", "tle here\x07", "\x07"},
			wantBell: []bool{false, false, true},
			wantMsgs: [][]string{nil, nil, nil},
		},
		{
			name:     "ESC split across chunk boundary",
			chunks:   []string{"\x1b", "]0;t\x07"},
			wantBell: []bool{false, false},
			wantMsgs: [][]string{nil, nil},
		},
		{
			name:     "CSI sequences do not affect detection",
			chunks:   []string{"\x1b[2J\x1b[31mred\x1b[0m\x07"},
			wantBell: []bool{true},
			wantMsgs: [][]string{nil},
		},
		{
			name:     "OSC 9 notification terminated by BEL",
			chunks:   []string{"\x1b]9;Approval requested\x07"},
			wantBell: []bool{false},
			wantMsgs: [][]string{{"Approval requested"}},
		},
		{
			name:     "OSC 9 notification terminated by ST",
			chunks:   []string{"\x1b]9;Codex turn complete\x1b\\"},
			wantBell: []bool{false},
			wantMsgs: [][]string{{"Codex turn complete"}},
		},
		{
			name:     "OSC 9 split across chunks",
			chunks:   []string{"\x1b]9;Appro", "val req", "uested\x07"},
			wantBell: []bool{false, false, false},
			wantMsgs: [][]string{nil, nil, {"Approval requested"}},
		},
		{
			name:     "bare bell and OSC 9 in one chunk",
			chunks:   []string{"\x07\x1b]9;needs input\x07"},
			wantBell: []bool{true},
			wantMsgs: [][]string{{"needs input"}},
		},
		{
			name:     "OSC 0 then OSC 9 resets buffer between sequences",
			chunks:   []string{"\x1b]0;title\x07\x1b]9;msg\x07"},
			wantBell: []bool{false},
			wantMsgs: [][]string{{"msg"}},
		},
		{
			name:     "OSC 99 is not a notification",
			chunks:   []string{"\x1b]99;not for us\x07"},
			wantBell: []bool{false},
			wantMsgs: [][]string{nil},
		},
		{
			name:     "empty OSC 9 payload produces no message",
			chunks:   []string{"\x1b]9;\x07"},
			wantBell: []bool{false},
			wantMsgs: [][]string{nil},
		},
		{
			name:     "oversized OSC 9 payload is capped without panic",
			chunks:   []string{"\x1b]9;" + strings.Repeat("a", maxOSCBuffer+200) + "\x07"},
			wantBell: []bool{false},
			wantMsgs: [][]string{{strings.Repeat("a", maxOSCBuffer-2)}},
		},
		{
			name:     "invalid UTF-8 payload is sanitized",
			chunks:   []string{"\x1b]9;bad\xff\xfebytes\x07"},
			wantBell: []bool{false},
			wantMsgs: [][]string{{"bad�bytes"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &bellDetector{}
			for i, chunk := range tt.chunks {
				got := d.process([]byte(chunk))
				if got.bell != tt.wantBell[i] {
					t.Errorf("chunk %d: process(%q).bell = %v, want %v",
						i, chunk, got.bell, tt.wantBell[i])
				}
				if len(got.messages) != len(tt.wantMsgs[i]) {
					t.Fatalf("chunk %d: process(%q).messages = %q, want %q",
						i, chunk, got.messages, tt.wantMsgs[i])
				}
				for j, msg := range got.messages {
					if msg != tt.wantMsgs[i][j] {
						t.Errorf("chunk %d message %d = %q, want %q",
							i, j, msg, tt.wantMsgs[i][j])
					}
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

func TestEventHubRecent(t *testing.T) {
	hub := NewEventHub()

	if got := hub.Recent(0); len(got) != 0 {
		t.Fatalf("Recent(0) on empty hub = %d events, want 0", len(got))
	}

	// Publish more than the cap; history must stay bounded and ordered
	for i := 0; i < maxRecentEvents+20; i++ {
		hub.Publish(Event{
			SessionID: "s",
			Type:      EventTypeBell,
			Timestamp: time.Unix(int64(i), 0),
		})
	}

	all := hub.Recent(0)
	if len(all) != maxRecentEvents {
		t.Fatalf("Recent(0) = %d events, want %d", len(all), maxRecentEvents)
	}
	// Newest first: first entry has the largest timestamp
	if !all[0].Timestamp.After(all[1].Timestamp) {
		t.Errorf("Recent not newest-first: %v then %v",
			all[0].Timestamp, all[1].Timestamp)
	}

	limited := hub.Recent(5)
	if len(limited) != 5 {
		t.Fatalf("Recent(5) = %d events, want 5", len(limited))
	}
	if !limited[0].Timestamp.Equal(all[0].Timestamp) {
		t.Error("Recent(5) does not start with the newest event")
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
