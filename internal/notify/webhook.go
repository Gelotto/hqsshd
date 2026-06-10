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

// Package notify delivers session events to external push services.
package notify

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gelotto/hqsshd/internal/logging"
	"github.com/gelotto/hqsshd/internal/session"
)

// WebhookNotifier POSTs session events to a configured URL.
//
// The request format is ntfy-compatible (plain-text body with Title,
// Priority, and Tags headers), so the URL can be an https://ntfy.sh/<topic>
// endpoint for app-dead phone push, or any custom relay that reads the
// X-HQSSH-* headers for structured data.
type WebhookNotifier struct {
	url    string
	client *http.Client
	done   chan struct{}
}

// NewWebhookNotifier creates a notifier targeting url.
func NewWebhookNotifier(url string) *WebhookNotifier {
	return &WebhookNotifier{
		url:    url,
		client: &http.Client{Timeout: 10 * time.Second},
		done:   make(chan struct{}),
	}
}

// Start subscribes to the hub and delivers events until Stop is called.
func (w *WebhookNotifier) Start(hub *session.EventHub) {
	id, ch := hub.Subscribe()

	go func() {
		defer hub.Unsubscribe(id)
		logging.Info("webhook notifier started", "url", w.url)

		for {
			select {
			case <-w.done:
				return
			case event, ok := <-ch:
				if !ok {
					return
				}
				w.send(event)
			}
		}
	}()
}

// Stop terminates event delivery.
func (w *WebhookNotifier) Stop() {
	close(w.done)
}

// send POSTs a single event; failures are logged, never retried — events
// are advisory notifications.
func (w *WebhookNotifier) send(event session.Event) {
	name := event.SessionName
	if name == "" {
		name = event.Tool
	}
	if name == "" {
		name = event.SessionID
	}

	var body, title, priority, tags string
	switch event.Type {
	case session.EventTypeBell:
		body = fmt.Sprintf("%s needs attention", name)
		title = "Agent needs attention"
		priority = "high"
		tags = "bell"
	case session.EventTypeEnded:
		body = fmt.Sprintf("%s ended", name)
		title = "Session ended"
		priority = "default"
		tags = "checkered_flag"
	default:
		return
	}

	req, err := http.NewRequest(http.MethodPost, w.url, strings.NewReader(body))
	if err != nil {
		logging.Warn("webhook request build failed", "error", err)
		return
	}
	// ntfy-compatible notification headers
	req.Header.Set("Title", title)
	req.Header.Set("Priority", priority)
	req.Header.Set("Tags", tags)
	// Structured data for custom relays
	req.Header.Set("X-HQSSH-Session-ID", event.SessionID)
	req.Header.Set("X-HQSSH-Tool", event.Tool)
	req.Header.Set("X-HQSSH-Event", event.Type.String())

	resp, err := w.client.Do(req)
	if err != nil {
		logging.Warn("webhook delivery failed", "error", err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode >= 300 {
		logging.Warn("webhook delivery rejected",
			"status", resp.StatusCode,
			"url", w.url,
		)
	} else {
		logging.Debug("webhook delivered",
			"event_type", event.Type.String(),
			"session_id", event.SessionID,
		)
	}
}
