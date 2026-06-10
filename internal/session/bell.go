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

// bellDetector detects bare BEL (0x07) characters in a terminal output
// stream.
//
// AI CLIs like Claude Code ring the terminal bell when they finish a task or
// need user input. However, BEL is also the terminator for OSC escape
// sequences (e.g. `ESC ] 0 ; title BEL` used to set the window title), so a
// naive byte scan would false-fire on every title update. This detector
// tracks OSC state across chunks and only reports bells outside sequences.
//
// Not safe for concurrent use; each session owns one detector and feeds it
// from its single PTY reader goroutine.
type bellDetector struct {
	afterEscape bool
	inOSC       bool
}

const (
	bellByte        = 0x07
	escByte         = 0x1b
	oscIntroducer    = ']'  // after ESC starts an OSC sequence
	stringTerminator = '\\' // after ESC ends an OSC sequence (ST)
)

// process scans a chunk of terminal output and reports whether it contains
// at least one attention bell (a BEL outside any OSC sequence). State
// persists across calls so sequences split across chunk boundaries are
// handled correctly.
func (d *bellDetector) process(data []byte) bool {
	found := false
	for _, b := range data {
		if d.afterEscape {
			d.afterEscape = false
			if b == oscIntroducer {
				d.inOSC = true
			} else if d.inOSC && b == stringTerminator {
				d.inOSC = false
			}
			continue
		}
		if b == escByte {
			d.afterEscape = true
			continue
		}
		if b == bellByte {
			if d.inOSC {
				d.inOSC = false
			} else {
				found = true
			}
		}
	}
	return found
}
