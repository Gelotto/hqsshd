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
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	"github.com/gelotto/hqsshd/internal/config"
	"github.com/gelotto/hqsshd/internal/logging"
	"github.com/gelotto/hqsshd/internal/pidfile"
	"github.com/gelotto/hqsshd/internal/servicemgr"
	pb "github.com/gelotto/hqsshd/proto"
)

// Startup tunables. Variables so tests can shorten them.
var (
	// tcpBindTimeout bounds how long Listen retries a busy TCP port before
	// serving socket-only and retrying in the background. An old instance
	// draining its shutdown holds the port for a second or two; anything
	// longer is another program, and readiness must not wait for it.
	tcpBindTimeout = 5 * time.Second
	// tcpBindInterval is the pause between TCP bind attempts.
	tcpBindInterval = 500 * time.Millisecond
	// socketProbeTimeout bounds the "is someone already listening?" dial.
	socketProbeTimeout = time.Second
	// tcpRetryInterval is how often a daemon that is serving socket-only
	// (port still held by something else) retries the TCP bind.
	tcpRetryInterval = 5 * time.Second
)

// shutdownGracePeriod bounds how long Stop waits for in-flight RPCs before
// forcibly terminating the gRPC server. Streams that would otherwise never
// end (Input, WatchEvents) are told to return when Stop begins, so this is
// only hit by a genuinely stuck client. It must stay well under the service
// manager's kill deadline (launchd ExitTimeOut / systemd TimeoutStopSec,
// both 30s as installed) — launchd's *default* is 5s.
const shutdownGracePeriod = 5 * time.Second

// ErrShuttingDown is returned by Listen (and makes Serve return nil) when
// Stop was called before startup completed — a SIGTERM that lands in the
// milliseconds between NewServer and Listen. It is a clean exit, not an
// error.
var ErrShuttingDown = errors.New("shutdown requested before startup completed")

// AlreadyRunningError is returned by Listen when another hqsshd owns the
// socket (or a live pidfile). Exit code 3.
type AlreadyRunningError struct {
	PID    int    // 0 when unknown
	Socket string // socket path in use
	Hint   string // how to stop the running instance
}

func (e *AlreadyRunningError) Error() string {
	where := "socket " + e.Socket + " is in use"
	if e.Socket == "" {
		where = "it holds the pidfile lock"
	}
	if e.PID > 0 {
		return fmt.Sprintf("hqsshd is already running (pid %d, %s)\nStop it first: %s", e.PID, where, e.Hint)
	}
	return fmt.Sprintf("hqsshd is already running (%s)\nStop it first: %s", where, e.Hint)
}

// BindError is returned by Listen when a listener cannot be created. Exit
// code 4.
type BindError struct {
	Addr string
	Err  error
	Hint string
}

func (e *BindError) Error() string {
	return fmt.Sprintf("bind %s: %v\n%s", e.Addr, e.Err, e.Hint)
}

func (e *BindError) Unwrap() error { return e.Err }

// preflight is the single-instance check. It runs from NewServer BEFORE any
// store is loaded, because loading mutates shared state (sessions.json is
// rewritten, stale temp files are swept) and a second instance must not
// touch the live daemon's data on its way to exit 3.
func (s *Server) preflight() error {
	socketPath := s.config.Socket
	if err := validateSocketPath(socketPath); err != nil {
		return err
	}

	// The pidfile lock is the guard: an flock held for the daemon's
	// lifetime, released by the kernel when the process dies. A stale file
	// from a crash or a reboot is simply taken over.
	lock, holder, err := pidfile.Acquire(s.pidPath, os.Getpid())
	switch {
	case errors.Is(err, pidfile.ErrLocked):
		return &AlreadyRunningError{PID: holder, Hint: servicemgr.StopHintFor(holder)}
	case err != nil:
		logging.Warn("cannot lock pidfile; single-instance protection disabled", "path", s.pidPath, "error", err)
	default:
		s.pidLock = lock
	}

	// Probe the socket before touching it: a connect that succeeds means a
	// daemon is serving (one older than v1.4.0 holds no pidfile lock), and
	// unlinking its socket out from under it would leave two daemons and a
	// phone app that reaches neither reliably.
	if err := s.prepareSocketPath(socketPath); err != nil {
		s.releasePidLock()
		return err
	}
	return nil
}

// Listen creates the Unix socket and TCP listeners and builds the gRPC
// server. It does not accept connections; call Serve for that. Everything
// a service manager or a user needs to know about a failed start is in the
// returned error's message.
func (s *Server) Listen() error {
	// Listen and Stop are mutually exclusive: Stop runs on the signal
	// goroutine and must see either nothing bound or everything bound.
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.isStopping() {
		return ErrShuttingDown
	}
	socketPath := s.config.Socket

	unixListener, err := net.Listen("unix", socketPath)
	if err != nil && errors.Is(err, syscall.EADDRINUSE) {
		// A socket file appeared since preflight (a daemon that crashed in
		// the meantime, or a probe/bind race): re-probe, refuse if live,
		// remove if stale, and bind once more.
		if perr := s.prepareSocketPath(socketPath); perr != nil {
			s.releasePidLock()
			return perr
		}
		unixListener, err = net.Listen("unix", socketPath)
	}
	if err != nil {
		s.releasePidLock()
		return &BindError{Addr: socketPath, Err: err,
			Hint: "check that " + filepath.Dir(socketPath) + " exists and is writable, or set a different path with --socket"}
	}
	// Go unlinks a Unix socket path when the listener closes. With two
	// daemons in flight (restart, upgrade) that lets the OLD instance delete
	// the NEW instance's socket as it exits. We remove the path ourselves,
	// and only when it is still ours (see removeSocketIfOwned).
	if ul, ok := unixListener.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		unixListener.Close()
		os.Remove(socketPath)
		s.releasePidLock()
		return &BindError{Addr: socketPath, Err: err, Hint: "could not restrict socket permissions"}
	}
	s.socketInfo, _ = os.Stat(socketPath)

	// TCP listener for SSH-tunnelled clients (the mobile app). A port still
	// held by an exiting instance frees up within seconds; a port held by
	// something else is a configuration problem, but not one worth a
	// restart loop: serve the socket, keep retrying in the background, and
	// let `hqssh doctor` / `hqssh service status` report the missing
	// transport. Any other bind error is fatal.
	var tcpListener net.Listener
	if s.config.TCPPort > 0 {
		tcpAddr := fmt.Sprintf("127.0.0.1:%d", s.config.TCPPort)
		tcpListener, err = listenTCPWithRetry(tcpAddr)
		switch {
		case err == nil:
		case errors.Is(err, syscall.EADDRINUSE):
			logging.Error("TCP port in use; serving the Unix socket only and retrying in the background — the mobile app cannot connect until the port is free",
				"addr", tcpAddr, "error", err,
				"hint", fmt.Sprintf("find the owner with `lsof -nP -iTCP:%d`, or set tcp_port in ~/.hqssh/daemon.yaml", s.config.TCPPort))
			s.tcpPending = tcpAddr
		default:
			unixListener.Close()
			s.removeSocketIfOwned()
			s.releasePidLock()
			return &BindError{Addr: tcpAddr, Err: err,
				Hint: fmt.Sprintf("cannot listen on port %d; set tcp_port in ~/.hqssh/daemon.yaml (0 disables TCP; the mobile app requires it)", s.config.TCPPort)}
		}
	}

	s.unixListener = unixListener
	s.tcpListener = tcpListener
	s.grpcServer = s.buildGRPCServer()
	s.listening = true
	return nil
}

func (s *Server) releasePidLock() {
	if s.pidLock != nil {
		if err := s.pidLock.Release(); err != nil {
			logging.Warn("cannot release pidfile", "path", s.pidPath, "error", err)
		}
		s.pidLock = nil
	}
}

func (s *Server) isStopping() bool {
	select {
	case <-s.stopping:
		return true
	default:
		return false
	}
}

// Serve accepts connections until Stop is called. Listen must have
// succeeded first.
func (s *Server) Serve() error {
	s.mu.Lock()
	if !s.listening {
		s.mu.Unlock()
		return errors.New("Serve called before a successful Listen")
	}
	if s.isStopping() {
		s.mu.Unlock()
		return nil // Stop already ran; nothing to serve
	}
	grpcServer, unixListener, tcpListener := s.grpcServer, s.unixListener, s.tcpListener

	// Serve TCP in the background; errors are logged rather than lost. The
	// WaitGroup Add happens under the lock so Stop's Wait cannot race it.
	switch {
	case tcpListener != nil:
		s.tcpWg.Add(1)
		go func() {
			defer s.tcpWg.Done()
			if err := grpcServer.Serve(tcpListener); err != nil {
				logging.Info("TCP listener stopped", "error", err)
			}
		}()
	case s.tcpPending != "":
		s.tcpWg.Add(1)
		go s.retryTCP(s.tcpPending, grpcServer)
	}
	s.mu.Unlock()

	// Serve the Unix socket in the foreground (blocks until shutdown)
	return grpcServer.Serve(unixListener)
}

// retryTCP keeps trying to bind addr until it succeeds or the daemon
// stops, then serves on it. Runs under tcpWg.
func (s *Server) retryTCP(addr string, grpcServer *grpc.Server) {
	defer s.tcpWg.Done()
	for {
		select {
		case <-s.stopping:
			return
		case <-time.After(tcpRetryInterval):
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		s.mu.Lock()
		if s.isStopping() {
			s.mu.Unlock()
			ln.Close()
			return
		}
		s.tcpListener = ln
		s.tcpPending = ""
		s.mu.Unlock()
		logging.Info("TCP listener bound after retry", "addr", addr)
		if err := grpcServer.Serve(ln); err != nil {
			logging.Info("TCP listener stopped", "error", err)
		}
		return
	}
}

// Addrs returns the bound socket path and TCP address ("" when disabled,
// pending, or not yet listening).
func (s *Server) Addrs() (socket, tcp string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.listening {
		return "", ""
	}
	socket = s.config.Socket
	if s.tcpListener != nil {
		tcp = s.tcpListener.Addr().String()
	}
	return socket, tcp
}

// TCPPending returns the TCP address the daemon is still trying to bind
// ("" when bound or disabled).
func (s *Server) TCPPending() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tcpPending
}

// Stopping is closed when Stop begins; long-lived handlers select on it so
// GracefulStop does not have to wait for clients that never hang up.
func (s *Server) Stopping() <-chan struct{} {
	return s.stopping
}

// Stop shuts the daemon down: state is persisted first, the socket path is
// released so a replacement can bind at once, then sessions and streams are
// closed and the gRPC server drained under a deadline. Safe to call more
// than once, and before Listen.
func (s *Server) Stop() {
	s.stopOnce.Do(s.stop)
}

func (s *Server) stop() {
	// Excludes a concurrent Listen/Serve (see Listen). Held for the whole
	// shutdown: nothing that runs during it needs the lock.
	s.mu.Lock()
	defer s.mu.Unlock()

	logging.Info("hqsshd stopping")

	// Persist what cannot change during shutdown while there is still
	// plenty of time budget — a SIGKILL later must not lose it.
	s.saveStore("project registry", s.registry.Save)
	s.saveStore("discovery state", s.discoveryState.Save)

	// Tell Input/WatchEvents handlers to return
	close(s.stopping)

	// Release the socket path immediately: the listener keeps serving
	// connections it already accepted, new clients get "no such file"
	// (which the CLI reports as "restarting"), and the replacement daemon
	// can bind without fighting us.
	s.removeSocketIfOwned()

	// Stop webhook delivery first (sessions closing below emit ENDED events)
	if s.webhook != nil {
		s.webhook.Stop()
	}

	// Cancel running tasks, then persist their final state
	if s.taskExecutor != nil {
		s.taskExecutor.Close()
	}
	s.saveStore("task store", s.taskStore.Save)
	s.saveStore("task run store", s.taskRunStore.Save)

	// Close session manager (kills all sessions, saves the session store),
	// then end WatchEvents streams -- GracefulStop below waits for active
	// RPCs, and those streams never finish on their own
	if s.sessionManager != nil {
		s.sessionManager.Close()
		s.sessionManager.Events().Close()
	}

	if s.grpcServer != nil {
		stopped := make(chan struct{})
		go func() {
			s.grpcServer.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(shutdownGracePeriod):
			logging.Warn("graceful stop timed out, forcing stop", "timeout", shutdownGracePeriod)
			s.grpcServer.Stop()
			<-stopped
		}
		s.tcpWg.Wait()
	}

	// Normally a no-op (removed above); covers the path where Serve never
	// ran and the listener close could not unlink (SetUnlinkOnClose false)
	s.removeSocketIfOwned()
	s.releasePidLock()

	logging.Info("hqsshd stopped")
}

func (s *Server) saveStore(name string, save func() error) {
	if save == nil {
		return
	}
	if err := save(); err != nil {
		logging.Warn("failed to save "+name, "error", err)
	}
}

// removeSocketIfOwned unlinks the socket path only while it is still the
// one this process bound — same inode and same creation instant, so a
// replacement's socket that happens to reuse the inode number is never
// touched.
func (s *Server) removeSocketIfOwned() {
	if s.socketInfo == nil || s.config == nil {
		return
	}
	cur, err := os.Stat(s.config.Socket)
	if err != nil {
		return
	}
	if os.SameFile(cur, s.socketInfo) && cur.ModTime().Equal(s.socketInfo.ModTime()) {
		os.Remove(s.config.Socket)
	}
	s.socketInfo = nil // never look at the path again: whatever appears next is not ours
}

// validateSocketPath rejects paths the kernel cannot bind, with a message
// that explains the limit instead of "bind: invalid argument".
func validateSocketPath(path string) error {
	if path == "" {
		return &BindError{Addr: "(empty)", Err: errors.New("no socket path configured"),
			Hint: "set 'socket:' in ~/.hqssh/daemon.yaml or pass --socket"}
	}
	if max := servicemgr.MaxSocketPathLen(); len(path) > max {
		return &BindError{Addr: path, Err: syscall.ENAMETOOLONG,
			Hint: fmt.Sprintf("socket path is %d bytes; this OS allows at most %d for Unix sockets. "+
				"Set a shorter path with --socket or the 'socket:' key in ~/.hqssh/daemon.yaml (default %s)",
				len(path), max, config.DefaultSocketPath)}
	}
	return nil
}

// prepareSocketPath makes sure nothing is listening at path and removes a
// stale socket file left by a daemon that was killed without cleaning up.
func (s *Server) prepareSocketPath(path string) error {
	conn, err := net.DialTimeout("unix", path, socketProbeTimeout)
	if err == nil {
		conn.Close()
		return &AlreadyRunningError{Socket: path, Hint: servicemgr.StopHintFor(0)}
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case errors.Is(err, syscall.ECONNREFUSED):
		logging.Info("removing stale socket", "socket", path)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return &BindError{Addr: path, Err: err, Hint: "remove the stale socket file by hand"}
		}
		return nil
	default:
		return &BindError{Addr: path, Err: err,
			Hint: "check permissions on " + filepath.Dir(path) + " and the socket file"}
	}
}

// listenTCPWithRetry binds addr, waiting out a port still held by an
// exiting instance. Any error other than "address in use" fails at once.
func listenTCPWithRetry(addr string) (net.Listener, error) {
	deadline := time.Now().Add(tcpBindTimeout)
	warned := false
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) || time.Now().After(deadline) {
			return nil, err
		}
		if !warned {
			logging.Warn("TCP port in use, retrying", "addr", addr, "timeout", tcpBindTimeout)
			warned = true
		}
		time.Sleep(tcpBindInterval)
	}
}

// buildGRPCServer assembles the gRPC server with keepalive, optional auth,
// all four services, and optional reflection.
func (s *Server) buildGRPCServer() *grpc.Server {
	serverOpts := []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    20 * time.Second, // Send ping every 20s if idle
			Timeout: 5 * time.Second,  // Wait 5s for pong
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second, // Minimum time between client pings
			PermitWithoutStream: true,             // Allow pings even when no streams
		}),
	}

	if s.config.AuthToken != "" {
		serverOpts = append(serverOpts,
			grpc.UnaryInterceptor(s.authUnaryInterceptor),
			grpc.StreamInterceptor(s.authStreamInterceptor),
		)
		logging.Info("token-based authentication enabled")
	}

	srv := grpc.NewServer(serverOpts...)

	pb.RegisterSystemServiceServer(srv, &systemService{server: s})
	pb.RegisterProjectServiceServer(srv, &projectService{server: s})
	pb.RegisterSessionServiceServer(srv, newSessionService(s))
	pb.RegisterTaskServiceServer(srv, &taskService{server: s})

	// Reflection exposes the full API schema: handy with grpcurl, off by
	// default in production.
	if s.config.EnableReflection {
		reflection.Register(srv)
		logging.Warn("gRPC reflection enabled (disable in production)")
	}

	return srv
}
