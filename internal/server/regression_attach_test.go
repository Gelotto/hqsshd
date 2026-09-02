package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/gelotto/hqsshd/internal/session"
	pb "github.com/gelotto/hqsshd/proto"
)

// slowAttachStream is a gatedAttachStream whose Send takes a little time,
// like a real link: output queues up behind it while the process finishes.
type slowAttachStream struct {
	*gatedAttachStream
	perSend time.Duration
}

func (s *slowAttachStream) Send(m *pb.TerminalOutput) error {
	time.Sleep(s.perSend)
	return s.gatedAttachStream.Send(m)
}

// When the process exits, its last chunks are usually still queued for the
// attached client: the reader broadcasts them and then closes Done, and
// Attach's select used to pick Done at random and return with output still
// queued. The tail (TAIL-MARK) must reach the client.
func TestAttachDropsQueuedOutputWhenSessionEnds(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a shell")
	}
	srv, _ := newTestServer(t, t.TempDir())

	sess, err := srv.sessionManager.Create("", "shell", "/tmp", "", nil, 80, 24)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &slowAttachStream{gatedAttachStream: newGatedAttachStream(ctx), perSend: 2 * time.Millisecond}
	attachDone := make(chan error, 1)
	go func() {
		attachDone <- newSessionService(srv).Attach(&pb.AttachRequest{SessionId: sess.ID, Cols: 80, Rows: 24}, stream)
	}()

	if _, err := sess.Write([]byte("echo RE''ADY\n")); err != nil {
		t.Fatal(err)
	}
	if !stream.waitFor(10*time.Second, func(m []*pb.TerminalOutput) bool {
		return strings.Contains(textAfter(m, 0), "READY")
	}) {
		t.Fatal("shell did not become ready")
	}

	// A burst that ends with the process exiting. 400 lines stay well under
	// the per-client queue (1024), so nothing is dropped by backpressure;
	// only the Done race could lose the tail.
	if _, err := sess.Write([]byte("i=0; while [ $i -lt 400 ]; do i=$((i+1)); echo LINE-$i; done; echo TAIL''-MARK; exit\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sess.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("shell did not exit")
	}

	select {
	case err := <-attachDone:
		if err != nil {
			t.Fatalf("Attach returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Attach did not return after the session ended")
	}

	stream.mu.Lock()
	all := textAfter(stream.msgs, 0)
	n := len(stream.msgs)
	stream.mu.Unlock()
	scroll, _ := srv.sessionManager.GetScrollback(sess.ID)
	inScrollback := strings.Contains(string(scroll), "TAIL-MARK")
	t.Logf("client got %d messages; TAIL-MARK in scrollback=%v, delivered to client=%v", n, inScrollback, strings.Contains(all, "TAIL-MARK"))
	if inScrollback && !strings.Contains(all, "TAIL-MARK") {
		t.Errorf("session's final output was in the scrollback but never streamed to the attached client (Attach returned on Done() with output still queued)")
	}
}

// Attach used to send the whole scrollback (up to max_scrollback_size =
// 10MB) as a single message, which a grpc-go client with default options
// (4 MiB) refuses. The replay is now chunked.
func TestAttachScrollbackOver4MBRejectedByDefaultClient(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a shell and pumps 5MB through the PTY")
	}
	srv, cfg := newTestServer(t, t.TempDir())
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()

	sess, err := srv.sessionManager.Create("", "shell", "/tmp", "", nil, 80, 24)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := sess.Write([]byte("head -c 5000000 /dev/zero | tr '\\0' x; echo BI''G-DONE\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		data, _ := srv.sessionManager.GetScrollback(sess.ID)
		if len(data) > 4*1024*1024+200*1024 && strings.Contains(string(data[len(data)-4096:]), "BIG-DONE") {
			t.Logf("scrollback is %d bytes", len(data))
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scrollback only reached %d bytes", len(data))
		}
		time.Sleep(100 * time.Millisecond)
	}

	conn, err := grpc.NewClient("unix:"+cfg.Socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewSessionServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	stream, err := client.Attach(ctx, &pb.AttachRequest{SessionId: sess.ID, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	t.Logf("first Attach Recv with default client options: %v", err)
	if status.Code(err) == codes.ResourceExhausted {
		t.Errorf("Attach replay rejected by a default grpc-go client: %v", err)
	}

	// GetScrollback is unary and cannot be chunked without a proto change;
	// the hqssh CLI raises its receive limit instead. Only Attach is asserted.
}

type fakeLogStream struct {
	grpc.ServerStream
	ctx  context.Context
	data []byte
}

func (f *fakeLogStream) Context() context.Context { return f.ctx }
func (f *fakeLogStream) Send(m *pb.TerminalOutput) error {
	f.data = append(f.data, m.GetData()...)
	return nil
}

// GetSessionLog must not read outside logs/sessions for any session_id.
func TestGetSessionLogPathTraversal(t *testing.T) {
	home := t.TempDir()
	srv, _ := newTestServer(t, home)
	logDir := srv.sessionManager.GetLogDir()
	if err := os.MkdirAll(logDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Sibling of the log dir, and a file in the log dir itself (in-dir ids
	// with slashes are fine, they just never match).
	parent := filepath.Dir(logDir)
	if err := os.WriteFile(filepath.Join(parent, "escape.log"), []byte("SECRET-PARENT"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "ok.log"), []byte("OK-CONTENT"), 0600); err != nil {
		t.Fatal(err)
	}
	svc := newSessionService(srv)

	cases := []struct {
		id       string
		wantCode codes.Code
	}{
		{"", codes.InvalidArgument},
		{"../escape", codes.NotFound},
		{"./../escape", codes.NotFound},
		{"..", codes.NotFound},
		{".", codes.NotFound},
		{"/etc/passwd", codes.NotFound},
		{filepath.Join(parent, "escape"), codes.NotFound},
		{"a/b", codes.NotFound},
		{"ok\x00", codes.NotFound},
		{"ok", codes.OK},
	}
	for _, c := range cases {
		st := &fakeLogStream{ctx: context.Background()}
		err := svc.GetSessionLog(&pb.GetSessionLogRequest{SessionId: c.id}, st)
		if got := status.Code(err); got != c.wantCode {
			t.Errorf("id %q: code %v (%v), want %v", c.id, got, err, c.wantCode)
		}
		if strings.Contains(string(st.data), "SECRET-PARENT") {
			t.Errorf("id %q escaped the log directory: %q", c.id, st.data)
		}
		if c.id == "ok" && string(st.data) != "OK-CONTENT" {
			t.Errorf("id ok: data %q", st.data)
		}
	}
}

// The limit is applied by store.ListEnded before the project filter, so a
// project-scoped query can return fewer records than exist for it.
func TestListHistoricalSessionsLimitBeforeProjectFilter(t *testing.T) {
	srv, _ := newTestServer(t, t.TempDir())
	st := srv.sessionManager.GetStore()
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 3; i++ { // older, project A
		st.Add(&session.SessionRecord{ID: "a" + string(rune('0'+i)), ProjectID: "A", Tool: "claude", Status: "ended",
			Created: base, Ended: base.Add(time.Duration(i) * time.Minute)})
	}
	for i := 0; i < 3; i++ { // newer, project B
		st.Add(&session.SessionRecord{ID: "b" + string(rune('0'+i)), ProjectID: "B", Tool: "claude", Status: "ended",
			Created: base, Ended: base.Add(time.Duration(10+i) * time.Minute)})
	}
	resp, err := newSessionService(srv).ListHistoricalSessions(context.Background(),
		&pb.ListHistoricalSessionsRequest{Limit: 2, ProjectId: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(resp.GetSessions()); got != 2 {
		t.Errorf("limit=2 project=A returned %d sessions, want 2 (3 exist for A)", got)
	}
}

// Create validates the working directory with os.Stat + IsNotExist only: a
// regular file passes validation and fails at fork/exec as Internal.
func TestCreateWorkingDirectoryIsRegularFile(t *testing.T) {
	srv, cfg := newTestServer(t, t.TempDir())
	cfg.EnableShellTool = true
	file := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := newSessionService(srv).Create(context.Background(),
		&pb.CreateSessionRequest{Tool: "shell", WorkingDirectory: file})
	t.Logf("Create(working_directory=<regular file>) -> %v", err)
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", status.Code(err))
	}
}
