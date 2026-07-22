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

package notify

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gelotto/hqsshd/internal/session"
)

type capturedRequest struct {
	body    string
	headers http.Header
}

func TestWebhookNotifierDeliversEvents(t *testing.T) {
	received := make(chan capturedRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			received <- capturedRequest{body: string(body), headers: r.Header.Clone()}
			w.WriteHeader(http.StatusOK)
		}))
	defer server.Close()

	hub := session.NewEventHub()
	notifier := NewWebhookNotifier(server.URL)
	notifier.Start(hub)
	defer notifier.Stop()

	hub.Publish(session.Event{
		SessionID:   "abc-123",
		SessionName: "myapp/claude-ab12",
		Tool:        "claude",
		Type:        session.EventTypeBell,
		Timestamp:   time.Now(),
	})

	select {
	case req := <-received:
		if req.body != "myapp/claude-ab12 needs attention" {
			t.Errorf("bell body = %q", req.body)
		}
		if got := req.headers.Get("Title"); got != "Agent needs attention" {
			t.Errorf("Title header = %q", got)
		}
		if got := req.headers.Get("Priority"); got != "high" {
			t.Errorf("Priority header = %q", got)
		}
		if got := req.headers.Get("X-HQSSH-Event"); got != "bell" {
			t.Errorf("X-HQSSH-Event header = %q", got)
		}
		if got := req.headers.Get("X-HQSSH-Session-ID"); got != "abc-123" {
			t.Errorf("X-HQSSH-Session-ID header = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bell event was not delivered")
	}

	hub.Publish(session.Event{
		SessionID:   "abc-456",
		SessionName: "myapp/codex-cd34",
		Tool:        "codex",
		Type:        session.EventTypeBell,
		Timestamp:   time.Now(),
		Message:     "Approval requested",
	})

	select {
	case req := <-received:
		if req.body != "myapp/codex-cd34: Approval requested" {
			t.Errorf("bell-with-message body = %q", req.body)
		}
		if got := req.headers.Get("Title"); got != "Agent needs attention" {
			t.Errorf("Title header = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bell event with message was not delivered")
	}

	hub.Publish(session.Event{
		SessionID:   "abc-123",
		SessionName: "myapp/claude-ab12",
		Tool:        "claude",
		Type:        session.EventTypeEnded,
		Timestamp:   time.Now(),
	})

	select {
	case req := <-received:
		if req.body != "myapp/claude-ab12 ended" {
			t.Errorf("ended body = %q", req.body)
		}
		if got := req.headers.Get("X-HQSSH-Event"); got != "ended" {
			t.Errorf("X-HQSSH-Event header = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ended event was not delivered")
	}
}

func TestWebhookNotifierStop(t *testing.T) {
	received := make(chan capturedRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			received <- capturedRequest{body: string(body)}
			w.WriteHeader(http.StatusOK)
		}))
	defer server.Close()

	hub := session.NewEventHub()
	notifier := NewWebhookNotifier(server.URL)
	notifier.Start(hub)
	notifier.Stop()

	// Give the goroutine time to unsubscribe, then verify no delivery
	deadline := time.After(2 * time.Second)
	for hub.SubscriberCount() != 0 {
		select {
		case <-deadline:
			t.Fatal("notifier did not unsubscribe after Stop")
		case <-time.After(10 * time.Millisecond):
		}
	}

	hub.Publish(session.Event{
		SessionID: "x",
		Type:      session.EventTypeBell,
		Timestamp: time.Now(),
	})

	select {
	case req := <-received:
		t.Errorf("unexpected delivery after Stop: %q", req.body)
	case <-time.After(300 * time.Millisecond):
		// Expected: nothing delivered
	}
}
