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
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// modeTracker tracks DEC private mode state (`ESC [ ? Pm h/l`) in a terminal
// output stream so attach replay can restore terminal state after the
// mode-setting sequences have been trimmed from the scrollback ring buffer.
//
// Full-screen TUIs (Claude Code, vim, htop) enable the alternate screen
// buffer and mouse tracking once at startup. The scrollback buffer is
// front-trimmed, so on long sessions those sequences are lost and a freshly
// attached client's terminal would stay in the normal buffer with mouse
// reporting off — breaking scroll gestures and input modes entirely.
//
// Safe for concurrent use: process runs on the PTY reader goroutine while
// preamble is called from attach handlers.
type modeTracker struct {
	mu sync.Mutex

	// Parser state, persisted across chunks so sequences split on chunk
	// boundaries are handled correctly.
	state    int
	params   []byte
	overflow bool

	// Mutually exclusive mode groups: the last-enabled code wins, disabling
	// any code in the group clears it (matching xterm semantics).
	altBuffer     int // 0 = main buffer, else 47/1047/1049
	mouseTracking int // 0 = off, else 1000/1001/1002/1003
	mouseEncoding int // 0 = default encoding, else 1005/1006/1015
	// Independent boolean modes (see trackedBoolModes), present when set.
	bools map[int]bool
}

const (
	msGround = iota
	msEscape
	msCSI
)

// Longest parameter run we bother accumulating; real mode sequences are far
// shorter, and anything longer is not something we track.
const maxCSIParams = 32

// Independent boolean modes restored on attach, in emission order:
// application cursor keys, autowrap, cursor visibility, focus reporting,
// bracketed paste.
var trackedBoolModes = []int{1, 7, 25, 1004, 2004}

// Modes that are enabled by default in a fresh terminal.
var defaultOnModes = map[int]bool{7: true, 25: true}

// process scans a chunk of terminal output for DEC private mode changes.
func (t *modeTracker) process(data []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, b := range data {
		switch t.state {
		case msGround:
			if b == escByte {
				t.state = msEscape
			}
		case msEscape:
			switch b {
			case '[':
				t.state = msCSI
				t.params = t.params[:0]
				t.overflow = false
			case escByte:
				// Stay in escape state
			default:
				t.state = msGround
			}
		case msCSI:
			switch {
			case b == escByte:
				// Aborted sequence, a new one starts
				t.state = msEscape
			case b >= 0x40 && b <= 0x7e:
				// Final byte terminates the sequence
				if !t.overflow && (b == 'h' || b == 'l') {
					t.apply(t.params, b == 'h')
				}
				t.state = msGround
			default:
				// Parameter or intermediate byte
				if len(t.params) < maxCSIParams {
					t.params = append(t.params, b)
				} else {
					t.overflow = true
				}
			}
		}
	}
}

// apply updates tracked state for a complete `CSI ? Pm h/l` sequence.
// Non-private sequences (no leading '?') are ignored.
func (t *modeTracker) apply(params []byte, enabled bool) {
	if len(params) == 0 || params[0] != '?' {
		return
	}
	for _, p := range strings.Split(string(params[1:]), ";") {
		mode, err := strconv.Atoi(p)
		if err != nil {
			continue
		}
		switch mode {
		case 47, 1047, 1049:
			t.altBuffer = pick(enabled, mode)
		case 1000, 1001, 1002, 1003:
			t.mouseTracking = pick(enabled, mode)
		case 1005, 1006, 1015:
			t.mouseEncoding = pick(enabled, mode)
		case 1, 7, 25, 1004, 2004:
			if t.bools == nil {
				t.bools = make(map[int]bool)
			}
			t.bools[mode] = enabled
		}
	}
}

func pick(enabled bool, mode int) int {
	if enabled {
		return mode
	}
	return 0
}

// preamble returns escape sequences that bring a fresh terminal to the
// session's current mode state. Empty when every tracked mode is at its
// default, so plain shell sessions get no preamble at all.
func (t *modeTracker) preamble() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()

	var b bytes.Buffer
	for _, mode := range trackedBoolModes {
		if v, ok := t.bools[mode]; ok && v != defaultOnModes[mode] {
			final := 'l'
			if v {
				final = 'h'
			}
			fmt.Fprintf(&b, "\x1b[?%d%c", mode, final)
		}
	}
	if t.altBuffer != 0 {
		fmt.Fprintf(&b, "\x1b[?%dh", t.altBuffer)
	}
	if t.mouseTracking != 0 {
		fmt.Fprintf(&b, "\x1b[?%dh", t.mouseTracking)
	}
	if t.mouseEncoding != 0 {
		fmt.Fprintf(&b, "\x1b[?%dh", t.mouseEncoding)
	}
	return b.Bytes()
}
