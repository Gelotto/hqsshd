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
	"fmt"
	"sync"
	"testing"
	"time"
)

// A consumer that is merely slow must never lose output: the bounded wait in
// broadcast is flow control, not a drop policy.
func TestBroadcast_SlowConsumerLosesNothing(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 1<<20)
	ch := sess.AddClient("c", 2)

	const n = 50
	var got []string
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			data := <-ch
			got = append(got, string(data))
			time.Sleep(clientSendTimeout / 4)
		}
	}()

	for i := 0; i < n; i++ {
		sess.appendAndBroadcast([]byte(fmt.Sprintf("chunk-%02d", i)))
	}
	wg.Wait()

	for i, s := range got {
		if want := fmt.Sprintf("chunk-%02d", i); s != want {
			t.Fatalf("chunk %d = %q, want %q (order or data lost)", i, s, want)
		}
	}
	if sess.TakeOutputGap("c") {
		t.Error("gap reported for a consumer that only ran slow")
	}
	sess.clientsMu.RLock()
	drops := sess.clients["c"].dropCount
	sess.clientsMu.RUnlock()
	if drops != 0 {
		t.Errorf("dropCount = %d, want 0", drops)
	}
}

// A consumer that has stopped reading costs the PTY reader at most the
// bounded wait per chunk, after which the chunk is dropped and a gap is owed.
func TestBroadcast_StalledConsumerDropsWithinBound(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 1<<20)
	sess.AddClient("c", 1)

	sess.broadcast([]byte("a")) // fills the queue

	start := time.Now()
	sess.broadcast([]byte("b")) // must time out, not block forever
	elapsed := time.Since(start)

	if elapsed < clientSendTimeout {
		t.Errorf("broadcast returned after %v, before the %v bound", elapsed, clientSendTimeout)
	}
	if elapsed > 3*clientSendTimeout {
		t.Errorf("broadcast blocked %v, far past the %v bound", elapsed, clientSendTimeout)
	}
	if !sess.TakeOutputGap("c") {
		t.Fatal("no gap owed after a dropped chunk")
	}
	if sess.TakeOutputGap("c") {
		t.Error("gap reported twice")
	}
}

// The gap marker must land exactly where the gap is: after the last chunk
// that got through, before the first chunk that fits again.
func TestBroadcast_GapMarkerPositioned(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 1<<20)
	ch := sess.AddClient("c", 1)

	sess.broadcast([]byte("a"))
	sess.broadcast([]byte("b")) // dropped: queue holds "a", consumer absent

	if got := string(<-ch); got != "a" {
		t.Fatalf("first chunk = %q, want a", got)
	}

	// Queue has room for one chunk now: the owed marker takes it, and "c"
	// must then wait (bounded) for the consumer to make room.
	done := make(chan struct{})
	go func() {
		sess.broadcast([]byte("c"))
		close(done)
	}()

	marker := <-ch
	if len(marker) != 0 {
		t.Fatalf("expected the gap marker after the drop, got %q", marker)
	}
	if got := string(<-ch); got != "c" {
		t.Fatalf("chunk after marker = %q, want c", got)
	}
	<-done
	if sess.TakeOutputGap("c") {
		t.Error("gap still pending after the marker was delivered")
	}
}

// Removing a client while the reader is inside the bounded wait must not
// panic (the channel is closed under it) or deadlock.
func TestBroadcast_RemoveClientWhileBlocked(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 1<<20)
	sess.AddClient("c", 1)
	sess.broadcast([]byte("a"))

	go func() {
		time.Sleep(clientSendTimeout / 4)
		sess.RemoveClient("c")
	}()

	done := make(chan struct{})
	go func() {
		sess.broadcast([]byte("b"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * clientSendTimeout):
		t.Fatal("broadcast did not return after the client was removed")
	}
}

func TestTakeOutputGap_UnknownClient(t *testing.T) {
	sess := NewSession("proj-1", "shell", "/tmp", "", nil, 80, 24, 1<<20)
	if sess.TakeOutputGap("nope") {
		t.Error("gap reported for an unknown client")
	}
}
