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

package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/gelotto/hqsshd/internal/config"
	pb "github.com/gelotto/hqsshd/proto"
)

// gatedAttachStream is a pb.SessionService_AttachServer whose Send blocks
// while the gate is closed, simulating a client whose link has stalled.
type gatedAttachStream struct {
	grpc.ServerStream
	ctx context.Context

	mu    sync.Mutex
	gate  chan struct{} // receive succeeds when open (channel closed)
	msgs  []*pb.TerminalOutput
	wakes chan struct{}
}

func newGatedAttachStream(ctx context.Context) *gatedAttachStream {
	open := make(chan struct{})
	close(open) // closed gate = Send passes
	return &gatedAttachStream{ctx: ctx, gate: open, wakes: make(chan struct{}, 1)}
}

func (s *gatedAttachStream) Context() context.Context { return s.ctx }

func (s *gatedAttachStream) Send(m *pb.TerminalOutput) error {
	s.mu.Lock()
	gate := s.gate
	s.mu.Unlock()
	select {
	case <-gate:
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
	s.mu.Lock()
	s.msgs = append(s.msgs, m)
	s.mu.Unlock()
	select {
	case s.wakes <- struct{}{}:
	default:
	}
	return nil
}

func (s *gatedAttachStream) setOpen(open bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if open {
		// Close the CURRENT gate so a Send already parked on it wakes up;
		// replacing it would strand that waiter on the old channel forever.
		select {
		case <-s.gate: // already closed (open)
		default:
			close(s.gate)
		}
	} else {
		s.gate = make(chan struct{})
	}
}

// waitFor blocks until pred holds over the messages received so far.
func (s *gatedAttachStream) waitFor(timeout time.Duration, pred func(msgs []*pb.TerminalOutput) bool) bool {
	deadline := time.After(timeout)
	for {
		s.mu.Lock()
		ok := pred(s.msgs)
		s.mu.Unlock()
		if ok {
			return true
		}
		select {
		case <-s.wakes:
		case <-deadline:
			return false
		}
	}
}

func textAfter(msgs []*pb.TerminalOutput, from int) string {
	var b strings.Builder
	for i := from; i < len(msgs); i++ {
		b.Write(msgs[i].GetData())
	}
	return b.String()
}

// A client whose stream stalls long enough to overflow its (tiny) queue must
// receive an output_gap message where the loss happened, and once its
// backlog drains the process must be asked to repaint (SIGWINCH).
func TestAttachEmitsGapMarkerThenRepaints(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a shell and waits on bounded broadcast timeouts")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.DefaultConfig()
	cfg.Tools = nil
	cfg.Socket = shortSocketPath(t)
	cfg.TCPPort = 0
	cfg.Sessions.ClientBufferSize = 1 // overflow almost immediately
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(srv.Stop)

	sess, err := srv.sessionManager.Create("", "shell", "/tmp", "", nil, 80, 24)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newGatedAttachStream(ctx)

	attachDone := make(chan error, 1)
	go func() {
		attachDone <- newSessionService(srv).Attach(
			&pb.AttachRequest{SessionId: sess.ID, Cols: 80, Rows: 24}, stream)
	}()

	// Install a WINCH trap so a repaint is observable, and wait for it.
	if _, err := sess.Write([]byte("trap 'echo RE''PAINT-MARK' WINCH; echo TRAP''-READY\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !stream.waitFor(10*time.Second, func(m []*pb.TerminalOutput) bool {
		return strings.Contains(textAfter(m, 0), "TRAP-READY")
	}) {
		t.Fatal("shell did not become ready")
	}
	stream.mu.Lock()
	readyCount := len(stream.msgs)
	stream.mu.Unlock()

	// Stall the link, flood the PTY well past the two-chunk queue, and keep
	// the link stalled long enough for the bounded waits to give up.
	stream.setOpen(false)
	if _, err := sess.Write([]byte("yes FLOOD | head -c 200000; echo FL''OOD-DONE\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	time.Sleep(2500 * time.Millisecond)
	stream.setOpen(true)

	// The gap must be reported in-band ...
	if !stream.waitFor(10*time.Second, func(m []*pb.TerminalOutput) bool {
		for _, msg := range m[readyCount:] {
			if msg.GetOutputGap() {
				return true
			}
		}
		return false
	}) {
		t.Fatal("no output_gap message after the client's queue overflowed")
	}
	// ... and once the backlog drains the shell must have been told to repaint.
	if !stream.waitFor(10*time.Second, func(m []*pb.TerminalOutput) bool {
		return strings.Contains(textAfter(m, readyCount), "REPAINT-MARK")
	}) {
		t.Fatal("no repaint (SIGWINCH) after the gap was delivered")
	}

	cancel()
	select {
	case <-attachDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Attach did not return after the client context was cancelled")
	}
}
