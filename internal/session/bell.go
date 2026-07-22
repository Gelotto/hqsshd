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
	"bytes"
	"strings"
)

// bellDetector detects attention signals in a terminal output stream:
// bare BEL (0x07) characters and OSC 9 desktop notifications.
//
// AI CLIs signal "finished / needs input" in two ways. Claude Code rings the
// terminal bell (a bare BEL, via preferredNotifChannel terminal_bell). Codex
// emits an OSC 9 notification — `ESC ] 9 ; message BEL` — which carries the
// reason as text ("Approval requested", turn complete). BEL is also the
// terminator for other OSC sequences (e.g. `ESC ] 0 ; title BEL` used to set
// the window title), so a naive byte scan would false-fire on every title
// update. This detector tracks OSC state across chunks, reports bells outside
// sequences, and extracts OSC 9 payloads.
//
// Not safe for concurrent use; each session owns one detector and feeds it
// from its single PTY reader goroutine.
type bellDetector struct {
	afterEscape bool
	inOSC       bool
	oscBuf      []byte
}

// bellResult reports what a chunk of terminal output contained.
type bellResult struct {
	bell     bool     // at least one bare BEL outside any OSC sequence
	messages []string // OSC 9 notification payloads, in order of appearance
}

// attention reports whether the chunk contained any attention signal.
func (r bellResult) attention() bool { return r.bell || len(r.messages) > 0 }

// lastMessage returns the most recent OSC 9 payload, or "" if none.
func (r bellResult) lastMessage() string {
	if len(r.messages) == 0 {
		return ""
	}
	return r.messages[len(r.messages)-1]
}

const (
	bellByte         = 0x07
	escByte          = 0x1b
	oscIntroducer    = ']'  // after ESC starts an OSC sequence
	stringTerminator = '\\' // after ESC ends an OSC sequence (ST)

	// maxOSCBuffer caps payload accumulation so a malformed stream that never
	// terminates its OSC sequence cannot grow the buffer unbounded.
	maxOSCBuffer = 512
)

// oscNotificationPrefix identifies OSC 9 (iTerm2-style desktop notification).
var oscNotificationPrefix = []byte("9;")

// process scans a chunk of terminal output for attention signals. State
// persists across calls so sequences split across chunk boundaries are
// handled correctly.
func (d *bellDetector) process(data []byte) bellResult {
	var res bellResult
	for _, b := range data {
		if d.afterEscape {
			d.afterEscape = false
			if b == oscIntroducer {
				d.inOSC = true
				d.oscBuf = d.oscBuf[:0]
			} else if d.inOSC && b == stringTerminator {
				d.endOSC(&res)
			}
			continue
		}
		if b == escByte {
			d.afterEscape = true
			continue
		}
		if b == bellByte {
			if d.inOSC {
				d.endOSC(&res)
			} else {
				res.bell = true
			}
			continue
		}
		if d.inOSC && len(d.oscBuf) < maxOSCBuffer {
			d.oscBuf = append(d.oscBuf, b)
		}
	}
	return res
}

// endOSC closes the current OSC sequence, extracting the payload if it was a
// notification. Payloads are sanitized to valid UTF-8: the message crosses a
// proto3 string field, and gRPC marshalling rejects invalid UTF-8.
func (d *bellDetector) endOSC(res *bellResult) {
	d.inOSC = false
	if payload, ok := bytes.CutPrefix(d.oscBuf, oscNotificationPrefix); ok {
		msg := strings.TrimSpace(strings.ToValidUTF8(string(payload), "�"))
		if msg != "" {
			res.messages = append(res.messages, msg)
		}
	}
	d.oscBuf = d.oscBuf[:0]
}
