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
	"sync"
	"testing"
	"time"
)

func TestAttachClientReplayThenLive(t *testing.T) {
	sess := NewSession("", "shell", "", "test", nil, 80, 24, 0)

	sess.appendAndBroadcast([]byte("before-attach"))

	replay, ch := sess.AttachClient("client-1", 16)
	if !bytes.Contains(replay, []byte("before-attach")) {
		t.Errorf("replay missing prior output: %q", replay)
	}

	sess.appendAndBroadcast([]byte("after-attach"))

	select {
	case live := <-ch:
		if string(live) != "after-attach" {
			t.Errorf("live chunk = %q, want %q", live, "after-attach")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no live chunk received")
	}

	if bytes.Contains(replay, []byte("after-attach")) {
		t.Error("replay contains output produced after attach")
	}
}

func TestAttachClientIncludesModePreamble(t *testing.T) {
	sess := NewSession("", "claude", "", "test", nil, 80, 24, 0)

	// Simulate a TUI whose mode-set sequences were already trimmed from
	// the scrollback: the tracker saw them but the buffer holds later data.
	sess.modes.process([]byte("\x1b[?1049h\x1b[?1006h"))
	sess.appendAndBroadcast([]byte("screen content"))

	replay, _ := sess.AttachClient("client-1", 16)
	if !bytes.HasPrefix(replay, []byte("\x1b[?1049h\x1b[?1006h")) {
		t.Errorf("replay does not start with mode preamble: %q", replay[:min(len(replay), 30)])
	}
	if !bytes.HasSuffix(replay, []byte("screen content")) {
		t.Errorf("replay does not end with scrollback: %q", replay)
	}
}

// TestAttachClientNoGapOrDuplicate attaches repeatedly while a writer is
// streaming sequence-numbered chunks and checks that the first live chunk
// is exactly the successor of the last replayed chunk -- the regression
// this guards against is output falling between the scrollback snapshot
// and client registration (or being delivered twice).
func TestAttachClientNoGapOrDuplicate(t *testing.T) {
	const chunkLen = 8
	sess := NewSession("", "shell", "", "test", nil, 80, 24, 64*1024*1024)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			sess.appendAndBroadcast([]byte(fmt.Sprintf("%0*d", chunkLen, i)))
		}
	}()

	for attach := 0; attach < 50; attach++ {
		clientID := fmt.Sprintf("client-%d", attach)
		replay, ch := sess.AttachClient(clientID, 4096)

		var lastReplayed = -1
		if len(replay) >= chunkLen {
			if len(replay)%chunkLen != 0 {
				t.Fatalf("replay length %d not a multiple of chunk length", len(replay))
			}
			n, err := strconv.Atoi(string(replay[len(replay)-chunkLen:]))
			if err != nil {
				t.Fatalf("replay tail is not a counter: %v", err)
			}
			lastReplayed = n
		}

		select {
		case live := <-ch:
			first, err := strconv.Atoi(string(live[:chunkLen]))
			if err != nil {
				t.Fatalf("live chunk is not a counter: %v", err)
			}
			if lastReplayed >= 0 && first != lastReplayed+1 {
				t.Fatalf("attach %d: replay ended at %d but live stream started at %d (gap or duplicate)",
					attach, lastReplayed, first)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no live chunk received")
		}

		sess.RemoveClient(clientID)
	}

	close(stop)
	wg.Wait()
}

func TestScrollbackTrimRuneBoundary(t *testing.T) {
	// 8-byte buffer; appending "abc" + three 3-byte runes forces a trim
	// that lands mid-rune and must advance to the next rune start.
	sess := NewSession("", "shell", "", "test", nil, 80, 24, 8)

	sess.appendAndBroadcast([]byte("abc"))
	sess.appendAndBroadcast([]byte("日本語"))

	got := sess.GetScrollback()
	if string(got) != "本語" {
		t.Errorf("scrollback after trim = %q, want %q", got, "本語")
	}
	if len(got) > 0 && got[0]&0xC0 == 0x80 {
		t.Error("scrollback starts with a UTF-8 continuation byte")
	}
}
