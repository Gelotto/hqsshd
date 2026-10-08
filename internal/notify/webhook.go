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
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
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
	wg     sync.WaitGroup // the delivery goroutine

	ctx    context.Context // cancelled by Stop: aborts an in-flight POST
	cancel context.CancelFunc
}

// redactURL returns only the scheme and host of a webhook URL for logs.
// The path is the credential for ntfy (anyone who knows the topic can read
// and post to it), and the query or userinfo may carry tokens.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(unparseable URL)"
	}
	return u.Scheme + "://" + u.Host
}

// NewWebhookNotifier creates a notifier targeting url.
func NewWebhookNotifier(url string) *WebhookNotifier {
	ctx, cancel := context.WithCancel(context.Background())
	return &WebhookNotifier{
		url:    url,
		client: &http.Client{Timeout: 10 * time.Second},
		done:   make(chan struct{}),
		ctx:    ctx,
		cancel: cancel,
	}
}

// Start subscribes to the hub and delivers events until Stop is called.
func (w *WebhookNotifier) Start(hub *session.EventHub) {
	id, ch := hub.Subscribe()

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer hub.Unsubscribe(id)
		logging.Info("webhook notifier started", "url", redactURL(w.url))

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

// Stop terminates event delivery, aborting an in-flight POST, and waits
// for the delivery goroutine to exit.
func (w *WebhookNotifier) Stop() {
	close(w.done)
	w.cancel()
	w.wg.Wait()
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
		if event.Message != "" {
			body = fmt.Sprintf("%s: %s", name, event.Message)
		} else {
			body = fmt.Sprintf("%s needs attention", name)
		}
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

	req, err := http.NewRequestWithContext(w.ctx, http.MethodPost, w.url, strings.NewReader(body))
	if err != nil {
		logging.Warn("webhook request build failed", "url", redactURL(w.url))
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
		// *url.Error repeats the full URL; log only the cause
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		logging.Warn("webhook delivery failed", "url", redactURL(w.url), "error", err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode >= 300 {
		logging.Warn("webhook delivery rejected",
			"status", resp.StatusCode,
			"url", redactURL(w.url),
		)
	} else {
		logging.Debug("webhook delivered",
			"event_type", event.Type.String(),
			"session_id", event.SessionID,
		)
	}
}
