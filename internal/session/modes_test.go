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

import "testing"

// Claude Code's actual startup sequence (captured from claude 2.1.212):
// alt buffer, full mouse tracking with SGR encoding, bracketed paste,
// focus reporting.
const claudeStartup = "\x1b[?1049h\x1b[?2004h\x1b[?1004h\x1b[?1000h\x1b[?1002h\x1b[?1003h\x1b[?1006h"

func TestModeTracker(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   string
	}{
		{
			name:   "fresh session has no preamble",
			chunks: []string{"$ ls\r\nfile.txt\r\n"},
			want:   "",
		},
		{
			name:   "claude code startup",
			chunks: []string{claudeStartup},
			want:   "\x1b[?1004h\x1b[?2004h\x1b[?1049h\x1b[?1003h\x1b[?1006h",
		},
		{
			name: "sequence split across chunk boundaries",
			chunks: []string{
				"output\x1b", "[", "?10", "49", "h more",
			},
			want: "\x1b[?1049h",
		},
		{
			name:   "TUI exit restores defaults",
			chunks: []string{claudeStartup, "\x1b[?1006l\x1b[?1003l\x1b[?1002l\x1b[?1000l\x1b[?1004l\x1b[?2004l\x1b[?1049l"},
			want:   "",
		},
		{
			name:   "last mouse tracking mode wins",
			chunks: []string{"\x1b[?1003h\x1b[?1000h"},
			want:   "\x1b[?1000h",
		},
		{
			name:   "disabling any mouse mode turns tracking off",
			chunks: []string{"\x1b[?1003h\x1b[?1000l"},
			want:   "",
		},
		{
			name:   "multiple modes in one sequence",
			chunks: []string{"\x1b[?1000;1006h"},
			want:   "\x1b[?1000h\x1b[?1006h",
		},
		{
			name:   "hidden cursor is restored",
			chunks: []string{"\x1b[?25l"},
			want:   "\x1b[?25l",
		},
		{
			name:   "cursor hide then show needs no preamble",
			chunks: []string{"\x1b[?25l", "drawing...", "\x1b[?25h"},
			want:   "",
		},
		{
			name:   "application cursor keys",
			chunks: []string{"\x1b[?1h"},
			want:   "\x1b[?1h",
		},
		{
			name:   "non-private CSI h is ignored",
			chunks: []string{"\x1b[4h\x1b[20h"},
			want:   "",
		},
		{
			name:   "untracked private modes are ignored",
			chunks: []string{"\x1b[?2026h\x1b[?2031h\x1b[?12h"},
			want:   "",
		},
		{
			name:   "SGR color sequences do not confuse the parser",
			chunks: []string{"\x1b[38;2;255;193;7mhello\x1b[0m\x1b[?1049h"},
			want:   "\x1b[?1049h",
		},
		{
			name: "ESC aborts an unfinished sequence",
			chunks: []string{
				"\x1b[?104" + "\x1b[?1006h", // first sequence never terminated
			},
			want: "\x1b[?1006h",
		},
		{
			name:   "overlong parameter run is discarded",
			chunks: []string{"\x1b[?1049;0000000000000000000000000000000000000000h\x1b[?1000h"},
			want:   "\x1b[?1000h",
		},
		{
			name:   "alt buffer variant 47 is preserved as written",
			chunks: []string{"\x1b[?47h"},
			want:   "\x1b[?47h",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := &modeTracker{}
			for _, chunk := range tt.chunks {
				tracker.process([]byte(chunk))
			}
			if got := string(tracker.preamble()); got != tt.want {
				t.Errorf("preamble = %q, want %q", got, tt.want)
			}
		})
	}
}
