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
	"time"

	"github.com/gelotto/hqsshd/internal/logging"
	"github.com/google/uuid"
)

// EventType identifies the kind of session event.
type EventType int

const (
	EventTypeUnspecified EventType = iota
	// EventTypeBell fires when a session rings the terminal bell outside an
	// escape sequence — AI CLIs use this to signal "finished / needs input".
	EventTypeBell
	// EventTypeEnded fires when a session's process exits.
	EventTypeEnded
)

func (t EventType) String() string {
	switch t {
	case EventTypeBell:
		return "bell"
	case EventTypeEnded:
		return "ended"
	default:
		return "unspecified"
	}
}

// Event describes something that happened to a session, for clients that
// watch all sessions (e.g. the mobile app showing agent notifications).
type Event struct {
	SessionID   string
	SessionName string
	Tool        string
	Type        EventType
	Timestamp   time.Time
}

// maxRecentEvents bounds the in-memory event history ring buffer.
const maxRecentEvents = 100

// EventHub fans session events out to subscribers and keeps a bounded
// in-memory history for activity-feed queries.
//
// Subscribers receive on buffered channels; events are dropped (not blocked
// on) if a subscriber is slow, since events are advisory notifications.
type EventHub struct {
	mu          sync.RWMutex
	subscribers map[string]chan Event
	recent      []Event // oldest first, capped at maxRecentEvents
	closed      bool
}

// NewEventHub creates an empty event hub.
func NewEventHub() *EventHub {
	return &EventHub{
		subscribers: make(map[string]chan Event),
	}
}

// Subscribe registers a new subscriber and returns its ID and channel.
// Call Unsubscribe with the ID when done.
func (h *EventHub) Subscribe() (string, <-chan Event) {
	id := uuid.New().String()
	ch := make(chan Event, 64)

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		// Hub is shutting down: hand back a closed channel so the caller's
		// receive loop ends immediately instead of blocking forever.
		close(ch)
		return id, ch
	}
	h.subscribers[id] = ch
	h.mu.Unlock()

	logging.Debug("event subscriber added", "subscriber_id", id)
	return id, ch
}

// Unsubscribe removes a subscriber and closes its channel.
func (h *EventHub) Unsubscribe(id string) {
	h.mu.Lock()
	ch, ok := h.subscribers[id]
	if ok {
		delete(h.subscribers, id)
	}
	h.mu.Unlock()

	if ok {
		close(ch)
		logging.Debug("event subscriber removed", "subscriber_id", id)
	}
}

// Publish records an event in the history and delivers it to all
// subscribers, dropping it for any subscriber whose buffer is full.
func (h *EventHub) Publish(event Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return
	}

	h.recent = append(h.recent, event)
	if len(h.recent) > maxRecentEvents {
		h.recent = h.recent[len(h.recent)-maxRecentEvents:]
	}

	for id, ch := range h.subscribers {
		select {
		case ch <- event:
		default:
			logging.Warn("event dropped for slow subscriber",
				"subscriber_id", id,
				"session_id", event.SessionID,
				"event_type", event.Type.String(),
			)
		}
	}
}

// Close removes and closes every subscriber channel and stops accepting new
// subscribers. Used at daemon shutdown so WatchEvents streams end instead of
// keeping the gRPC server's graceful stop waiting forever. Safe to call
// multiple times.
func (h *EventHub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return
	}
	h.closed = true

	for id, ch := range h.subscribers {
		close(ch)
		delete(h.subscribers, id)
	}
}

// SubscriberCount returns the number of active subscribers.
func (h *EventHub) SubscriberCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subscribers)
}

// Recent returns up to limit recent events, newest first.
// limit <= 0 returns all retained events.
func (h *EventHub) Recent(limit int) []Event {
	h.mu.RLock()
	defer h.mu.RUnlock()

	n := len(h.recent)
	if limit <= 0 || limit > n {
		limit = n
	}

	// Copy newest-first
	result := make([]Event, limit)
	for i := 0; i < limit; i++ {
		result[i] = h.recent[n-1-i]
	}
	return result
}
