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
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/gelotto/hqsshd/internal/config"
	"github.com/gelotto/hqsshd/internal/pidfile"
	"github.com/gelotto/hqsshd/internal/project"
	pb "github.com/gelotto/hqsshd/proto"
)

// shortSocketPath returns a socket path under /tmp: macOS caps Unix socket
// paths at 104 bytes and t.TempDir() is longer than that.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hqssh-t-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// freePort grabs an ephemeral port and releases it.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// newTestServer builds a server whose data dir is a temp HOME and whose
// socket is short enough for macOS. TCP is disabled unless the test
// enables it.
func newTestServer(t *testing.T, home string) (*Server, *config.Config) {
	t.Helper()
	t.Setenv("HOME", home)
	cfg := config.DefaultConfig()
	cfg.Tools = nil
	cfg.Socket = shortSocketPath(t)
	cfg.TCPPort = 0
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(srv.Stop)
	return srv, cfg
}

func dialUnix(path string) error {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// leaveStaleSocket creates a socket file nobody listens on, as a SIGKILLed
// daemon would.
func leaveStaleSocket(t *testing.T, path string) {
	t.Helper()
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale socket file should exist: %v", err)
	}
}

func TestListenRemovesStaleSocket(t *testing.T) {
	// Stale before NewServer: preflight removes it
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.DefaultConfig()
	cfg.Tools = nil
	cfg.Socket = shortSocketPath(t)
	cfg.TCPPort = 0
	leaveStaleSocket(t, cfg.Socket)
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer over stale socket: %v", err)
	}
	t.Cleanup(srv.Stop)
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen over stale socket: %v", err)
	}
	if err := dialUnix(cfg.Socket); err != nil {
		t.Errorf("socket not dialable after Listen: %v", err)
	}
	sock, tcp := srv.Addrs()
	if sock != cfg.Socket || tcp != "" {
		t.Errorf("Addrs() = %q, %q", sock, tcp)
	}
}

func TestListenRemovesStaleSocketThatAppearedAfterPreflight(t *testing.T) {
	srv, cfg := newTestServer(t, t.TempDir())
	leaveStaleSocket(t, cfg.Socket) // between NewServer and Listen
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen over a late stale socket: %v", err)
	}
	if err := dialUnix(cfg.Socket); err != nil {
		t.Errorf("socket not dialable after Listen: %v", err)
	}
}

func TestListenRefusesLiveSocket(t *testing.T) {
	home := t.TempDir()
	a, cfg := newTestServer(t, home)
	if err := a.Listen(); err != nil {
		t.Fatalf("A.Listen: %v", err)
	}
	go a.Serve()

	// Same socket, same data dir: the pidfile lock refuses the second
	// instance before it loads a single store.
	bcfg := *cfg
	b, err := NewServer(&bcfg)
	var already *AlreadyRunningError
	if !errors.As(err, &already) {
		t.Fatalf("NewServer error = %v, want AlreadyRunningError", err)
	}
	if b != nil {
		t.Error("NewServer returned a server alongside the error")
	}
	if already.PID != os.Getpid() {
		t.Errorf("reported pid %d, want the holder %d", already.PID, os.Getpid())
	}
	if !strings.Contains(err.Error(), "already running") || !strings.Contains(err.Error(), "Stop it first") {
		t.Errorf("message = %q", err.Error())
	}

	// A is untouched: still dialable, pidfile still A's and still locked
	if err := dialUnix(cfg.Socket); err != nil {
		t.Errorf("A's socket was disturbed: %v", err)
	}
	if status, pid, _ := pidfile.Check(a.pidPath); status != pidfile.StatusAlive || pid != os.Getpid() {
		t.Errorf("A's pidfile = %v %d", status, pid)
	}
}

func TestPreflightRefusesLiveSocketWithoutPidfile(t *testing.T) {
	// A pre-1.4.0 daemon holds no pidfile lock: the socket probe catches it
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.DefaultConfig()
	cfg.Tools = nil
	cfg.Socket = shortSocketPath(t)
	cfg.TCPPort = 0
	old, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()

	_, err = NewServer(cfg)
	var already *AlreadyRunningError
	if !errors.As(err, &already) {
		t.Fatalf("NewServer = %v, want AlreadyRunningError", err)
	}
	if !strings.Contains(err.Error(), cfg.Socket) {
		t.Errorf("message should name the socket: %q", err.Error())
	}
	// The refused instance must not leave its own pidfile behind
	if _, err := os.Stat(pidfile.Path(config.DataDirFor(home))); !os.IsNotExist(err) {
		t.Error("refused instance left a pidfile")
	}
}

func TestListenSocketPathTooLong(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.Tools = nil
	cfg.TCPPort = 0
	cfg.Socket = "/tmp/" + strings.Repeat("x", 200) + ".sock"

	_, err := NewServer(cfg)
	var bind *BindError
	if !errors.As(err, &bind) {
		t.Fatalf("NewServer error = %v, want BindError", err)
	}
	if !strings.Contains(err.Error(), "at most") || !strings.Contains(err.Error(), "--socket") {
		t.Errorf("message = %q", err.Error())
	}
	if _, err := os.Stat(cfg.Socket); !os.IsNotExist(err) {
		t.Error("socket file created despite rejection")
	}
}

func TestListenTCPPortBusyServesSocketOnlyAndRecovers(t *testing.T) {
	srv, cfg := newTestServer(t, t.TempDir())
	holder, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.TCPPort = holder.Addr().(*net.TCPAddr).Port

	oldTimeout, oldRetry := tcpBindTimeout, tcpRetryInterval
	tcpBindTimeout, tcpRetryInterval = 200*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { tcpBindTimeout, tcpRetryInterval = oldTimeout, oldRetry })

	// A foreign port holder is not fatal: the socket is served, the port is
	// reported as pending and retried in the background.
	if err := srv.Listen(); err != nil {
		holder.Close()
		t.Fatalf("Listen with a busy port should serve socket-only, got %v", err)
	}
	if _, tcp := srv.Addrs(); tcp != "" {
		t.Errorf("tcp bound while the port is held: %q", tcp)
	}
	if srv.TCPPending() == "" {
		t.Error("TCPPending() empty while the port is held")
	}
	if err := dialUnix(cfg.Socket); err != nil {
		t.Errorf("socket not served: %v", err)
	}
	go srv.Serve()

	// Once the port frees up the daemon binds it without a restart
	holder.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, tcp := srv.Addrs(); tcp != "" {
			if !strings.HasSuffix(tcp, ":"+strconv.Itoa(cfg.TCPPort)) {
				t.Errorf("tcp addr = %q", tcp)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("TCP listener never bound after the port was released")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if srv.TCPPending() != "" {
		t.Error("TCPPending() still set after binding")
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.TCPPort), time.Second)
	if err != nil {
		t.Fatalf("recovered TCP listener not dialable: %v", err)
	}
	conn.Close()
	srv.Stop()
}

func TestListenTCPRetriesUntilFree(t *testing.T) {
	srv, cfg := newTestServer(t, t.TempDir())
	holder, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.TCPPort = holder.Addr().(*net.TCPAddr).Port

	old := tcpBindTimeout
	tcpBindTimeout = 5 * time.Second
	t.Cleanup(func() { tcpBindTimeout = old })

	go func() {
		time.Sleep(300 * time.Millisecond)
		holder.Close()
	}()

	start := time.Now()
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Error("Listen returned before the port was released")
	}
	_, tcp := srv.Addrs()
	if !strings.HasSuffix(tcp, ":"+strconv.Itoa(cfg.TCPPort)) {
		t.Errorf("tcp addr = %q", tcp)
	}
}

func TestStopRemovesOwnedSocketAndPidfile(t *testing.T) {
	srv, cfg := newTestServer(t, t.TempDir())
	cfg.TCPPort = freePort(t)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()

	if _, err := os.Stat(srv.pidPath); err != nil {
		t.Fatalf("pidfile missing after Listen: %v", err)
	}

	srv.Stop()

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after Stop")
	}
	if _, err := os.Stat(cfg.Socket); !os.IsNotExist(err) {
		t.Error("socket file still exists after Stop")
	}
	if _, err := os.Stat(srv.pidPath); !os.IsNotExist(err) {
		t.Error("pidfile still exists after Stop")
	}
}

func TestStopKeepsForeignSocketAndPidfile(t *testing.T) {
	a, cfg := newTestServer(t, t.TempDir())
	if err := a.Listen(); err != nil {
		t.Fatal(err)
	}
	go a.Serve()

	// A replacement daemon takes over the path and the pidfile
	os.Remove(cfg.Socket)
	replacement, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		t.Fatal(err)
	}
	replacement.(*net.UnixListener).SetUnlinkOnClose(false)
	defer func() { replacement.Close(); os.Remove(cfg.Socket) }()
	if err := pidfile.Write(a.pidPath, 1); err != nil {
		t.Fatal(err)
	}

	a.Stop()

	if err := dialUnix(cfg.Socket); err != nil {
		t.Errorf("replacement's socket was removed by the old instance: %v", err)
	}
	if pid, err := pidfile.Read(a.pidPath); err != nil || pid != 1 {
		t.Errorf("replacement's pidfile = %d, %v; want 1", pid, err)
	}
}

func TestStopReleasesPidfileLock(t *testing.T) {
	srv, _ := newTestServer(t, t.TempDir())
	if status, _, _ := pidfile.Check(srv.pidPath); status != pidfile.StatusAlive {
		t.Fatalf("pidfile not locked after NewServer: %v", status)
	}
	srv.Stop()
	if _, err := os.Stat(srv.pidPath); !os.IsNotExist(err) {
		t.Error("pidfile still exists after Stop")
	}
	// A new instance can take over immediately
	if lock, holder, err := pidfile.Acquire(srv.pidPath, 99); err != nil {
		t.Errorf("Acquire after Stop = holder %d, %v", holder, err)
	} else {
		lock.Release()
	}
}

func TestStopSavesRegistryFirst(t *testing.T) {
	home := t.TempDir()
	srv, _ := newTestServer(t, home)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()

	srv.registry.Add(&project.Project{ID: "p1", Name: "p1", Path: "/tmp/p1"})
	srv.Stop()

	data, err := os.ReadFile(filepath.Join(home, ".hqssh", "projects.json"))
	if err != nil {
		t.Fatalf("projects.json not written: %v", err)
	}
	if !strings.Contains(string(data), `"p1"`) {
		t.Errorf("projects.json = %s", data)
	}
	entries, _ := os.ReadDir(filepath.Join(home, ".hqssh"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestStopBeforeListenIsSafe(t *testing.T) {
	srv, cfg := newTestServer(t, t.TempDir())
	srv.Stop()
	srv.Stop() // idempotent
	if _, err := os.Stat(cfg.Socket); !os.IsNotExist(err) {
		t.Error("socket created by Stop")
	}
	if err := srv.Serve(); err == nil {
		t.Error("Serve before Listen should fail")
	}
}

func TestGetStatusReportsPidAndGetInfoCommit(t *testing.T) {
	srv, _ := newTestServer(t, t.TempDir())
	svc := &systemService{server: srv}

	st, err := svc.GetStatus(context.Background(), &pb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if int(st.GetPid()) != os.Getpid() {
		t.Errorf("Pid = %d, want %d", st.GetPid(), os.Getpid())
	}
	info, err := svc.GetInfo(context.Background(), &pb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if info.GetCommit() != config.Commit {
		t.Errorf("Commit = %q, want %q", info.GetCommit(), config.Commit)
	}
}

func TestInputStreamReturnsOnStop(t *testing.T) {
	srv, cfg := newTestServer(t, t.TempDir())
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()

	conn, err := grpc.NewClient("unix:"+cfg.Socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewSessionServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := client.Input(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Make sure the RPC is established server-side before stopping: the
	// server only sees the stream once the client has sent something.
	if err := stream.Send(&pb.TerminalInput{SessionId: "nope", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	// The unknown session makes the handler return NotFound; that is fine
	// for this test only if it happens after Stop. Give the send a moment
	// to arrive, then stop and time the shutdown.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	srv.Stop()
	elapsed := time.Since(start)

	if elapsed >= shutdownGracePeriod {
		t.Errorf("Stop took %v; an open Input stream held GracefulStop to its deadline", elapsed)
	}
	_, err = stream.CloseAndRecv()
	if err == nil {
		t.Fatal("stream ended without error")
	}
	if c := status.Code(err); c != codes.Unavailable && c != codes.Internal && c != codes.NotFound && c != codes.Canceled {
		t.Errorf("stream error code = %v (%v)", c, err)
	}
}

func TestInputStreamHeldOpenDoesNotDelayStop(t *testing.T) {
	srv, cfg := newTestServer(t, t.TempDir())
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()

	conn, err := grpc.NewClient("unix:"+cfg.Socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Open an Input stream and never send: the handler is parked in Recv.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := pb.NewSessionServiceClient(conn).Input(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Force the stream onto the wire without a message: header exchange
	// needs a flush, which Send would do; use a zero-data message with an
	// empty session id — the handler validates only after Recv returns.
	if err := stream.Send(&pb.TerminalInput{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	srv.Stop()
	if elapsed := time.Since(start); elapsed >= shutdownGracePeriod {
		t.Errorf("Stop took %v with an idle Input stream open", elapsed)
	}
}

func TestStopBeforeListenMakesListenAndServeClean(t *testing.T) {
	srv, cfg := newTestServer(t, t.TempDir())
	srv.Stop()

	err := srv.Listen()
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Listen after Stop = %v, want ErrShuttingDown", err)
	}
	if _, err := os.Stat(cfg.Socket); !os.IsNotExist(err) {
		t.Error("socket left behind by a Listen that gave up")
	}
	if _, err := os.Stat(srv.pidPath); !os.IsNotExist(err) {
		t.Error("pidfile left behind by a Listen that gave up")
	}
}

func TestStopIsAJoinForConcurrentCallers(t *testing.T) {
	srv, _ := newTestServer(t, t.TempDir())
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()

	// First caller starts the shutdown; a second concurrent caller must
	// not return before it completes (main relies on this to exit only
	// after the pidfile is gone and "hqsshd stopped" is logged).
	first := make(chan struct{})
	go func() { srv.Stop(); close(first) }()
	srv.Stop()
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("second Stop returned before the first finished")
	}
	if _, err := os.Stat(srv.pidPath); !os.IsNotExist(err) {
		t.Error("pidfile still present after Stop returned")
	}
}
