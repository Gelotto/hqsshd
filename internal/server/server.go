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
	"crypto/subtle"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	"github.com/gelotto/hqsshd/internal/config"
	"github.com/gelotto/hqsshd/internal/logging"
	"github.com/gelotto/hqsshd/internal/notify"
	"github.com/gelotto/hqsshd/internal/project"
	"github.com/gelotto/hqsshd/internal/session"
	"github.com/gelotto/hqsshd/internal/task"
	"github.com/gelotto/hqsshd/internal/tools"
	pb "github.com/gelotto/hqsshd/proto"
)

// Server holds the daemon state and starts the gRPC server
type Server struct {
	config         *config.Config
	grpcServer     *grpc.Server
	unixListener   net.Listener
	tcpListener    net.Listener
	tcpWg          sync.WaitGroup // Fix 6: Track TCP goroutine
	detector       *tools.Detector
	discovery      *project.Discovery
	registry       *project.Registry
	sessionManager *session.Manager
	taskStore      *task.Store
	taskRunStore   *task.RunStore
	taskExecutor   *task.Executor
	webhook        *notify.WebhookNotifier // nil when events.webhook_url unset
	dataDir        string
	startupTime    int64
}

// NewServer creates a new HQSSH server
func NewServer(cfg *config.Config) (*Server, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dataDir := homeDir + "/.hqssh"

	detector := tools.NewDetector(cfg)
	discovery := project.NewDiscovery(cfg, detector)
	registry := project.NewRegistry(dataDir)

	// Load existing project registry
	if err := registry.Load(); err != nil {
		logging.Warn("failed to load project registry", "error", err)
	}

	// Create session manager with config values
	sessionMgr := session.NewManager(
		cfg.Sessions.IdleTimeout,
		cfg.Sessions.MaxSessions,
		cfg.Sessions.HistorySize,
		dataDir,
		cfg.Sessions.LogDirectory,
		cfg.Sessions.LogRetentionDays,
		cfg.Sessions.ClientBufferSize,
		cfg.Sessions.MaxScrollbackSize,
	)

	// Create task store and run store
	taskStore := task.NewStore(dataDir)
	if err := taskStore.Load(); err != nil {
		logging.Warn("failed to load task store", "error", err)
	}

	taskRunStore := task.NewRunStore(dataDir, cfg.Tasks.MaxRunsPerTask)
	if err := taskRunStore.Load(); err != nil {
		logging.Warn("failed to load task run store", "error", err)
	}

	// Create task executor
	taskExecutor := task.NewExecutor(taskStore, taskRunStore, cfg.Tasks.MaxOutputSize, cfg.Tasks.MaxTimeout)

	// Push session events to a webhook (e.g. ntfy) when configured
	var webhook *notify.WebhookNotifier
	if cfg.Events.WebhookURL != "" {
		webhook = notify.NewWebhookNotifier(cfg.Events.WebhookURL)
		webhook.Start(sessionMgr.Events())
	}

	return &Server{
		config:         cfg,
		detector:       detector,
		discovery:      discovery,
		registry:       registry,
		sessionManager: sessionMgr,
		taskStore:      taskStore,
		taskRunStore:   taskRunStore,
		taskExecutor:   taskExecutor,
		webhook:        webhook,
		dataDir:        dataDir,
		startupTime:    time.Now().Unix(),
	}, nil
}

// Start starts the gRPC server on both Unix socket and TCP port
func (s *Server) Start() error {
	// Remove existing socket file if present
	socketPath := s.config.Socket
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing socket: %w", err)
	}

	// Create Unix socket listener (don't assign to struct until all setup succeeds)
	unixListener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on socket: %w", err)
	}

	// Set socket permissions (readable/writable by owner)
	if err := os.Chmod(socketPath, 0600); err != nil {
		unixListener.Close()
		return fmt.Errorf("failed to set socket permissions: %w", err)
	}

	// Create TCP listener if port is configured (for SSH tunnel forwarding)
	// If TCP port fails to bind, log a warning but continue with Unix socket only.
	var tcpListener net.Listener
	if s.config.TCPPort > 0 {
		tcpAddr := fmt.Sprintf("127.0.0.1:%d", s.config.TCPPort)
		tcpListener, err = net.Listen("tcp", tcpAddr)
		if err != nil {
			logging.Warn("TCP port unavailable, continuing with Unix socket only",
				"port", s.config.TCPPort,
				"error", err,
			)
		}
	}

	// All listeners created successfully - now assign to struct
	s.unixListener = unixListener
	s.tcpListener = tcpListener

	// Build gRPC server options
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

	// Add auth interceptors if token is configured
	if s.config.AuthToken != "" {
		serverOpts = append(serverOpts,
			grpc.UnaryInterceptor(s.authUnaryInterceptor),
			grpc.StreamInterceptor(s.authStreamInterceptor),
		)
		logging.Info("token-based authentication enabled")
	}

	// Create gRPC server
	s.grpcServer = grpc.NewServer(serverOpts...)

	// Create and register services
	systemSvc := &systemService{server: s}
	projectSvc := &projectService{server: s}
	sessionSvc := newSessionService(s)
	taskSvc := &taskService{server: s}

	pb.RegisterSystemServiceServer(s.grpcServer, systemSvc)
	pb.RegisterProjectServiceServer(s.grpcServer, projectSvc)
	pb.RegisterSessionServiceServer(s.grpcServer, sessionSvc)
	pb.RegisterTaskServiceServer(s.grpcServer, taskSvc)

	// Enable gRPC reflection only when explicitly configured (off by default).
	// Reflection exposes the full API schema, which is useful for debugging
	// but should be disabled in production.
	if s.config.EnableReflection {
		reflection.Register(s.grpcServer)
		logging.Warn("gRPC reflection enabled (disable in production)")
	}

	logging.Info("hqsshd starting",
		"version", config.DaemonVersion,
		"unix_socket", socketPath,
	)
	if s.tcpListener != nil {
		logging.Info("TCP listener started", "addr", fmt.Sprintf("127.0.0.1:%d", s.config.TCPPort))
	}

	// Start serving on TCP listener in background goroutine
	// Note: We can't truly verify startup before Serve() accepts first connection.
	// Instead, we log errors so they're not silently lost.
	if s.tcpListener != nil {
		s.tcpWg.Add(1)
		go func() {
			defer s.tcpWg.Done()
			if err := s.grpcServer.Serve(s.tcpListener); err != nil {
				logging.Info("TCP listener stopped", "error", err)
			}
		}()
	}

	// Start serving on Unix socket (blocks until shutdown)
	return s.grpcServer.Serve(s.unixListener)
}

// shutdownGracePeriod bounds how long Stop waits for in-flight RPCs before
// forcibly terminating the gRPC server.
const shutdownGracePeriod = 10 * time.Second

// Stop gracefully stops the server
func (s *Server) Stop() {
	// Stop webhook delivery first (sessions closing below emit ENDED events)
	if s.webhook != nil {
		s.webhook.Stop()
	}

	// Close task executor first (cancels all running tasks)
	if s.taskExecutor != nil {
		s.taskExecutor.Close()
	}

	// Close session manager (kills all sessions), then end WatchEvents
	// streams -- GracefulStop below waits for active RPCs, and those
	// streams never finish on their own
	if s.sessionManager != nil {
		s.sessionManager.Close()
		s.sessionManager.Events().Close()
	}

	// Gracefully stop gRPC server (stops both listeners), bounded by a
	// deadline so a stuck client stream cannot hang shutdown forever
	if s.grpcServer != nil {
		stopped := make(chan struct{})
		go func() {
			s.grpcServer.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(shutdownGracePeriod):
			logging.Warn("graceful stop timed out, forcing stop",
				"timeout", shutdownGracePeriod,
			)
			s.grpcServer.Stop()
			<-stopped
		}
	}

	// Fix 6: Wait for TCP goroutine to finish
	s.tcpWg.Wait()

	// Clean up socket file
	if s.config != nil && s.config.Socket != "" {
		os.Remove(s.config.Socket)
	}

	// Save project registry
	if s.registry != nil {
		if err := s.registry.Save(); err != nil {
			logging.Warn("failed to save project registry", "error", err)
		}
	}

	// Save task store
	if s.taskStore != nil {
		if err := s.taskStore.Save(); err != nil {
			logging.Warn("failed to save task store", "error", err)
		}
	}

	// Save task run store
	if s.taskRunStore != nil {
		if err := s.taskRunStore.Save(); err != nil {
			logging.Warn("failed to save task run store", "error", err)
		}
	}
}

// isValidTool checks if a tool name is in the configured whitelist.
// The "shell" tool requires explicit opt-in via config.EnableShellTool.
// Tool names must also pass character validation.
func (s *Server) isValidTool(tool string) bool {
	// Validate characters first
	if !config.ValidateToolName(tool) {
		return false
	}
	if tool == "shell" {
		return s.config.EnableShellTool
	}
	for _, t := range s.config.Tools {
		if t.Name == tool {
			return true
		}
	}
	return false
}

// authUnaryInterceptor validates the auth token on unary RPCs.
func (s *Server) authUnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := s.validateAuth(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// authStreamInterceptor validates the auth token on streaming RPCs.
func (s *Server) authStreamInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := s.validateAuth(ss.Context()); err != nil {
		return err
	}
	return handler(srv, ss)
}

// validateAuth checks the authorization metadata header against the configured token.
func (s *Server) validateAuth(ctx context.Context) error {
	if s.config.AuthToken == "" {
		return nil // No auth configured
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}

	values := md.Get("authorization")
	if len(values) == 0 {
		return status.Error(codes.Unauthenticated, "missing authorization token")
	}

	token := values[0]
	// Support "Bearer <token>" format
	if len(token) >= 7 && token[:7] == "Bearer " {
		token = token[7:]
	}

	if subtle.ConstantTimeCompare([]byte(token), []byte(s.config.AuthToken)) != 1 {
		return status.Error(codes.Unauthenticated, "invalid authorization token")
	}
	return nil
}

// ============================================================================
// SystemService Implementation
// ============================================================================

type systemService struct {
	pb.UnimplementedSystemServiceServer
	server *Server
}

func (s *systemService) GetInfo(ctx context.Context, _ *pb.Empty) (*pb.SystemInfo, error) {
	hostname, _ := os.Hostname()
	installedTools := s.server.detector.DetectAll()

	return &pb.SystemInfo{
		Hostname:       hostname,
		Os:             runtime.GOOS,
		Arch:           runtime.GOARCH,
		DaemonVersion:  config.DaemonVersion,
		InstalledTools: installedTools,
	}, nil
}

func (s *systemService) GetStatus(ctx context.Context, _ *pb.Empty) (*pb.SystemStatus, error) {
	uptimeSeconds := time.Now().Unix() - s.server.startupTime

	return &pb.SystemStatus{
		UptimeSeconds:  uptimeSeconds,
		ActiveSessions: int32(s.server.sessionManager.Count()),
		ActiveProjects: int32(s.server.registry.Count()),
	}, nil
}

// ============================================================================
// ProjectService Implementation
// ============================================================================

type projectService struct {
	pb.UnimplementedProjectServiceServer
	server *Server
}

func (s *projectService) List(ctx context.Context, req *pb.ListProjectsRequest) (*pb.ListProjectsResponse, error) {
	projects := s.server.registry.List(req.GetFavoritesOnly())

	pbProjects := make([]*pb.Project, len(projects))
	for i, p := range projects {
		pbProjects[i] = projectToProto(p)
	}

	return &pb.ListProjectsResponse{
		Projects: pbProjects,
	}, nil
}

func (s *projectService) Add(ctx context.Context, req *pb.AddProjectRequest) (*pb.Project, error) {
	if req.GetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "path is required")
	}

	proj, err := s.server.discovery.CreateFromPath(req.GetPath(), req.GetName())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid path: %v", err)
	}

	s.server.registry.Add(proj)
	if err := s.server.registry.Save(); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to save registry: %v", err)
	}

	return projectToProto(proj), nil
}

func (s *projectService) Remove(ctx context.Context, req *pb.RemoveProjectRequest) (*pb.Empty, error) {
	if req.GetProjectId() == "" {
		return nil, status.Error(codes.InvalidArgument, "project_id is required")
	}

	if !s.server.registry.Remove(req.GetProjectId()) {
		return nil, status.Error(codes.NotFound, "project not found")
	}

	if err := s.server.registry.Save(); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to save registry: %v", err)
	}

	return &pb.Empty{}, nil
}

func (s *projectService) Discover(ctx context.Context, req *pb.DiscoverRequest) (*pb.DiscoverResponse, error) {
	discovered, totalScanned, err := s.server.discovery.Discover(req.GetDirectories(), int(req.GetMaxDepth()))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "discovery failed: %v", err)
	}

	// Merge with existing registry
	s.server.registry.MergeDiscovered(discovered)
	if err := s.server.registry.Save(); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to save registry: %v", err)
	}

	pbProjects := make([]*pb.Project, len(discovered))
	for i, p := range discovered {
		pbProjects[i] = projectToProto(p)
	}

	return &pb.DiscoverResponse{
		Discovered:   pbProjects,
		TotalScanned: int32(totalScanned),
	}, nil
}

func (s *projectService) GetTools(ctx context.Context, req *pb.GetToolsRequest) (*pb.GetToolsResponse, error) {
	if req.GetProjectId() == "" {
		return nil, status.Error(codes.InvalidArgument, "project_id is required")
	}

	proj := s.server.registry.Get(req.GetProjectId())
	if proj == nil {
		return nil, status.Error(codes.NotFound, "project not found")
	}

	detectedTools := s.server.detector.DetectForProject(proj.Path)

	return &pb.GetToolsResponse{
		Tools: detectedTools,
	}, nil
}

// ============================================================================
// SessionService Implementation
// ============================================================================

type sessionService struct {
	pb.UnimplementedSessionServiceServer
	server *Server
}

func newSessionService(server *Server) *sessionService {
	return &sessionService{
		server: server,
	}
}

func (s *sessionService) Create(ctx context.Context, req *pb.CreateSessionRequest) (*pb.Session, error) {
	tool := req.GetTool()
	if tool == "" {
		return nil, status.Error(codes.InvalidArgument, "tool is required")
	}
	if !s.server.isValidTool(tool) {
		return nil, status.Errorf(codes.InvalidArgument, "unknown tool: %q", tool)
	}

	// Determine working directory
	workingDir := req.GetWorkingDirectory()
	projectID := req.GetProjectId()

	if projectID != "" {
		// Get project path from registry
		proj := s.server.registry.Get(projectID)
		if proj == nil {
			return nil, status.Error(codes.NotFound, "project not found")
		}
		workingDir = proj.Path
	}

	if workingDir == "" {
		// Default to home directory
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to get home directory: %v", err)
		}
		workingDir = homeDir
	}

	// Validate working directory exists
	if _, err := os.Stat(workingDir); os.IsNotExist(err) {
		return nil, status.Errorf(codes.InvalidArgument, "working directory does not exist: %s", workingDir)
	}

	// Default terminal size with upper bounds
	cols := int(req.GetCols())
	rows := int(req.GetRows())
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	if cols > 500 {
		cols = 500
	}
	if rows > 200 {
		rows = 200
	}

	// Get optional tool arguments and name
	args := req.GetArgs()
	name := req.GetName()

	// Create session
	sess, err := s.server.sessionManager.Create(projectID, tool, workingDir, name, args, cols, rows)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create session: %v", err)
	}

	return sessionToProto(sess), nil
}

func (s *sessionService) List(ctx context.Context, req *pb.ListSessionsRequest) (*pb.ListSessionsResponse, error) {
	sessions := s.server.sessionManager.List(req.GetProjectId(), req.GetIncludeEnded())

	pbSessions := make([]*pb.Session, len(sessions))
	for i, sess := range sessions {
		pbSessions[i] = sessionToProto(sess)
	}

	return &pb.ListSessionsResponse{
		Sessions: pbSessions,
	}, nil
}

func (s *sessionService) Attach(req *pb.AttachRequest, stream pb.SessionService_AttachServer) error {
	sessionID := req.GetSessionId()
	if sessionID == "" {
		return status.Error(codes.InvalidArgument, "session_id is required")
	}

	cols := int(req.GetCols())
	rows := int(req.GetRows())
	// Clamp dimensions (same as Create)
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	if cols > 500 {
		cols = 500
	}
	if rows > 200 {
		rows = 200
	}

	// Attach to session
	clientID, outputCh, scrollback, err := s.server.sessionManager.Attach(sessionID, cols, rows)
	if err != nil {
		if strings.Contains(err.Error(), "has ended") {
			return status.Errorf(codes.FailedPrecondition, "session has ended")
		}
		return status.Errorf(codes.NotFound, "failed to attach: %v", err)
	}

	// Cleanup on exit
	defer func() {
		s.server.sessionManager.Detach(sessionID, clientID)
	}()

	// Send scrollback first (history catch-up)
	if len(scrollback) > 0 {
		if err := stream.Send(&pb.TerminalOutput{Data: scrollback}); err != nil {
			return status.Errorf(codes.Internal, "failed to send scrollback: %v", err)
		}
	}

	// Stream output to client
	sess := s.server.sessionManager.Get(sessionID)
	if sess == nil {
		return status.Error(codes.NotFound, "session not found")
	}

	for {
		select {
		case data, ok := <-outputCh:
			if !ok {
				// Channel closed - session ended
				return nil
			}
			if err := stream.Send(&pb.TerminalOutput{Data: data}); err != nil {
				return status.Errorf(codes.Internal, "failed to send output: %v", err)
			}

		case <-sess.Done():
			// Session ended
			return nil

		case <-stream.Context().Done():
			// Client disconnected
			return nil
		}
	}
}

func (s *sessionService) Input(stream pb.SessionService_InputServer) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(&pb.Empty{})
		}
		if err != nil {
			return status.Errorf(codes.Internal, "failed to receive input: %v", err)
		}

		sessionID := req.GetSessionId()
		if sessionID == "" {
			return status.Error(codes.InvalidArgument, "session_id is required")
		}

		data := req.GetData()
		if len(data) == 0 {
			continue
		}

		if err := s.server.sessionManager.Input(sessionID, data); err != nil {
			return status.Errorf(codes.Internal, "failed to send input: %v", err)
		}
	}
}

func (s *sessionService) Detach(ctx context.Context, req *pb.DetachRequest) (*pb.Empty, error) {
	// Detach is handled automatically when the Attach stream ends (the defer
	// in Attach calls sessionManager.Detach). This RPC exists for protocol
	// completeness but is effectively a no-op — the Attach stream's context
	// cancellation is the canonical detach mechanism. The client ID is only
	// known to the Attach goroutine that created it, so we cannot remove a
	// specific client here without additional tracking infrastructure.
	return &pb.Empty{}, nil
}

func (s *sessionService) Kill(ctx context.Context, req *pb.KillSessionRequest) (*pb.Empty, error) {
	sessionID := req.GetSessionId()
	if sessionID == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}

	if err := s.server.sessionManager.Kill(sessionID); err != nil {
		return nil, status.Errorf(codes.NotFound, "failed to kill session: %v", err)
	}

	return &pb.Empty{}, nil
}

func (s *sessionService) Resize(ctx context.Context, req *pb.ResizeRequest) (*pb.Empty, error) {
	sessionID := req.GetSessionId()
	if sessionID == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}

	cols := int(req.GetCols())
	rows := int(req.GetRows())

	if cols <= 0 || rows <= 0 {
		return nil, status.Error(codes.InvalidArgument, "cols and rows must be positive")
	}
	if cols > 500 || rows > 200 {
		return nil, status.Error(codes.InvalidArgument, "terminal dimensions too large (max 500x200)")
	}

	if err := s.server.sessionManager.Resize(sessionID, cols, rows); err != nil {
		return nil, status.Errorf(codes.NotFound, "failed to resize: %v", err)
	}

	return &pb.Empty{}, nil
}

func (s *sessionService) GetScrollback(ctx context.Context, req *pb.GetScrollbackRequest) (*pb.GetScrollbackResponse, error) {
	sessionID := req.GetSessionId()
	if sessionID == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}

	data, err := s.server.sessionManager.GetScrollback(sessionID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "failed to get scrollback: %v", err)
	}

	// If lines limit is specified, truncate to last N lines
	if req.GetLines() > 0 {
		data = lastNLines(data, int(req.GetLines()))
	}

	return &pb.GetScrollbackResponse{
		Data: data,
	}, nil
}

func (s *sessionService) GetSessionLog(req *pb.GetSessionLogRequest, stream pb.SessionService_GetSessionLogServer) error {
	sessionID := req.GetSessionId()
	if sessionID == "" {
		return status.Error(codes.InvalidArgument, "session_id is required")
	}

	logDir := s.server.sessionManager.GetLogDir()

	// Try to read the log file
	reader, err := session.ReadLog(logDir, sessionID)
	if err != nil {
		// If no log file, try to get scrollback from active session
		data, scrollErr := s.server.sessionManager.GetScrollback(sessionID)
		if scrollErr != nil {
			return status.Errorf(codes.NotFound, "session log not found: %v", err)
		}
		// Send scrollback as a single chunk
		if len(data) > 0 {
			if err := stream.Send(&pb.TerminalOutput{Data: data}); err != nil {
				return status.Errorf(codes.Internal, "failed to send data: %v", err)
			}
		}
		return nil
	}
	defer reader.Close()

	// Handle offset if specified
	// Note: Offset only works for uncompressed logs (active sessions).
	// Compressed (.gz) logs don't support seeking, so offset is ignored.
	if req.GetOffset() > 0 {
		if seeker, ok := reader.(io.Seeker); ok {
			if _, err := seeker.Seek(req.GetOffset(), io.SeekStart); err != nil {
				return status.Errorf(codes.Internal, "failed to seek: %v", err)
			}
		}
		// If reader doesn't support seeking (gzip), we continue from the beginning
	}

	// Stream log in chunks
	buf := make([]byte, 64*1024) // 64KB chunks
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&pb.TerminalOutput{Data: buf[:n]}); sendErr != nil {
				return status.Errorf(codes.Internal, "failed to send data: %v", sendErr)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return status.Errorf(codes.Internal, "failed to read log: %v", err)
		}
	}

	return nil
}

func (s *sessionService) ListHistoricalSessions(ctx context.Context, req *pb.ListHistoricalSessionsRequest) (*pb.ListHistoricalSessionsResponse, error) {
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 50
	}

	store := s.server.sessionManager.GetStore()
	logDir := s.server.sessionManager.GetLogDir()

	// Get ended sessions from store
	records := store.ListEnded(limit)

	// Filter by project if specified
	projectID := req.GetProjectId()
	if projectID != "" {
		filtered := make([]*session.SessionRecord, 0)
		for _, r := range records {
			if r.ProjectID == projectID {
				filtered = append(filtered, r)
			}
		}
		records = filtered
	}

	// Convert to proto
	pbSessions := make([]*pb.Session, len(records))
	for i, r := range records {
		// Get log size
		var logSize int64
		if size, err := session.GetLogSize(logDir, r.ID); err == nil {
			logSize = size
		}

		pbSessions[i] = &pb.Session{
			Id:               r.ID,
			Name:             r.Name,
			ProjectId:        r.ProjectID,
			Tool:             r.Tool,
			WorkingDirectory: r.WorkingDirectory,
			Status:           pb.SessionStatus_SESSION_STATUS_ENDED,
			CreatedAt:        r.Created.Unix(),
			EndedAt:          r.Ended.Unix(),
			LogPath:          r.LogPath,
			LogSizeBytes:     logSize,
		}
	}

	return &pb.ListHistoricalSessionsResponse{
		Sessions: pbSessions,
	}, nil
}

// WatchEvents streams session events (bell rung, session ended) to the
// client until it disconnects. Used by the mobile app to show agent
// notifications for sessions it is not attached to.
func (s *sessionService) WatchEvents(req *pb.WatchEventsRequest, stream pb.SessionService_WatchEventsServer) error {
	hub := s.server.sessionManager.Events()
	id, ch := hub.Subscribe()
	defer hub.Unsubscribe(id)

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(sessionEventToProto(event)); err != nil {
				return status.Errorf(codes.Internal, "failed to send event: %v", err)
			}
		}
	}
}

// ListEvents returns recent session events, newest first, for the agent
// activity feed.
func (s *sessionService) ListEvents(ctx context.Context, req *pb.ListEventsRequest) (*pb.ListEventsResponse, error) {
	events := s.server.sessionManager.Events().Recent(int(req.GetLimit()))

	pbEvents := make([]*pb.SessionEvent, len(events))
	for i, e := range events {
		pbEvents[i] = sessionEventToProto(e)
	}

	return &pb.ListEventsResponse{Events: pbEvents}, nil
}

// sessionEventToProto converts a session.Event to its protobuf form
func sessionEventToProto(e session.Event) *pb.SessionEvent {
	var t pb.SessionEventType
	switch e.Type {
	case session.EventTypeBell:
		t = pb.SessionEventType_SESSION_EVENT_TYPE_BELL
	case session.EventTypeEnded:
		t = pb.SessionEventType_SESSION_EVENT_TYPE_ENDED
	default:
		t = pb.SessionEventType_SESSION_EVENT_TYPE_UNSPECIFIED
	}
	return &pb.SessionEvent{
		SessionId:   e.SessionID,
		SessionName: e.SessionName,
		Tool:        e.Tool,
		Type:        t,
		Timestamp:   e.Timestamp.Unix(),
	}
}

// lastNLines returns the last n lines from a byte slice
func lastNLines(data []byte, n int) []byte {
	if len(data) == 0 || n <= 0 {
		return data
	}

	// Find newlines from the end
	lineCount := 0
	endIdx := len(data)

	for i := len(data) - 1; i >= 0; i-- {
		if data[i] == '\n' {
			lineCount++
			if lineCount > n {
				return data[i+1 : endIdx]
			}
		}
	}

	// Return all data if fewer than n lines
	return data
}

// ============================================================================
// TaskService Implementation
// ============================================================================

type taskService struct {
	pb.UnimplementedTaskServiceServer
	server *Server
}

func (s *taskService) List(ctx context.Context, req *pb.ListTasksRequest) (*pb.ListTasksResponse, error) {
	scope := protoToTaskScope(req.GetScope())
	tasks := s.server.taskStore.List(scope, req.GetProjectId())

	pbTasks := make([]*pb.Task, len(tasks))
	for i, t := range tasks {
		pbTasks[i] = taskToProto(t)
	}

	return &pb.ListTasksResponse{Tasks: pbTasks}, nil
}

func (s *taskService) Create(ctx context.Context, req *pb.CreateTaskRequest) (*pb.Task, error) {
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}

	tool := req.GetTool()
	if tool == "" {
		return nil, status.Error(codes.InvalidArgument, "tool is required")
	}
	if !s.server.isValidTool(tool) {
		return nil, status.Errorf(codes.InvalidArgument, "unknown tool: %q", tool)
	}

	scope := protoToTaskScope(req.GetScope())
	if scope == task.TaskScopeUnspecified {
		return nil, status.Error(codes.InvalidArgument, "scope is required")
	}

	// Validate project exists if scope is PROJECT
	if scope == task.TaskScopeProject {
		projectID := req.GetProjectId()
		if projectID == "" {
			return nil, status.Error(codes.InvalidArgument, "project_id is required for project-scoped tasks")
		}
		if s.server.registry.Get(projectID) == nil {
			return nil, status.Error(codes.NotFound, "project not found")
		}
	}

	// Global scope not supported in MVP
	if scope == task.TaskScopeGlobal {
		return nil, status.Error(codes.Unimplemented, "global tasks not yet supported")
	}

	// Interactive tasks not supported in MVP
	if req.GetInteractive() {
		return nil, status.Error(codes.Unimplemented, "interactive tasks not yet supported")
	}

	t := s.server.taskStore.Create(
		name,
		req.GetDescription(),
		tool,
		scope,
		req.GetProjectId(),
		req.GetPrompt(),
		req.GetInteractive(),
		int(req.GetTimeoutSeconds()),
	)

	if err := s.server.taskStore.Save(); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to save task store: %v", err)
	}

	return taskToProto(t), nil
}

func (s *taskService) Update(ctx context.Context, req *pb.UpdateTaskRequest) (*pb.Task, error) {
	id := req.GetId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}

	existing := s.server.taskStore.Get(id)
	if existing == nil {
		return nil, status.Error(codes.NotFound, "task not found")
	}

	// Validate tool if being updated
	if tool := req.GetTool(); tool != "" && !s.server.isValidTool(tool) {
		return nil, status.Errorf(codes.InvalidArgument, "unknown tool: %q", tool)
	}

	scope := protoToTaskScope(req.GetScope())
	if scope == task.TaskScopeUnspecified {
		scope = existing.Scope // Keep existing scope if not specified
	}

	// Global scope not supported
	if scope == task.TaskScopeGlobal {
		return nil, status.Error(codes.Unimplemented, "global tasks not yet supported")
	}

	// Interactive not supported
	if req.GetInteractive() {
		return nil, status.Error(codes.Unimplemented, "interactive tasks not yet supported")
	}

	t := s.server.taskStore.Update(
		id,
		req.GetName(),
		req.GetDescription(),
		req.GetTool(),
		scope,
		req.GetProjectId(),
		req.GetPrompt(),
		req.GetInteractive(),
		int(req.GetTimeoutSeconds()),
	)

	if t == nil {
		return nil, status.Error(codes.NotFound, "task not found")
	}

	if err := s.server.taskStore.Save(); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to save task store: %v", err)
	}

	return taskToProto(t), nil
}

func (s *taskService) Delete(ctx context.Context, req *pb.DeleteTaskRequest) (*pb.Empty, error) {
	taskID := req.GetTaskId()
	if taskID == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id is required")
	}

	if !s.server.taskStore.Delete(taskID) {
		return nil, status.Error(codes.NotFound, "task not found")
	}

	// Also delete associated runs
	s.server.taskRunStore.DeleteByTask(taskID)

	if err := s.server.taskStore.Save(); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to save task store: %v", err)
	}
	if err := s.server.taskRunStore.Save(); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to save task run store: %v", err)
	}

	return &pb.Empty{}, nil
}

func (s *taskService) Run(ctx context.Context, req *pb.RunTaskRequest) (*pb.TaskRun, error) {
	taskID := req.GetTaskId()
	if taskID == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id is required")
	}

	t := s.server.taskStore.Get(taskID)
	if t == nil {
		return nil, status.Error(codes.NotFound, "task not found")
	}

	if !s.server.isValidTool(t.Tool) {
		return nil, status.Errorf(codes.FailedPrecondition, "task has invalid tool %q (not in current config)", t.Tool)
	}

	// Determine project path for project-scoped tasks
	var projectPath string
	if t.Scope == task.TaskScopeProject && t.ProjectID != "" {
		proj := s.server.registry.Get(t.ProjectID)
		if proj == nil {
			return nil, status.Error(codes.NotFound, "task's project not found")
		}
		projectPath = proj.Path
	}

	run, err := s.server.taskExecutor.Run(taskID, projectPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to run task: %v", err)
	}

	// Save run store
	if err := s.server.taskRunStore.Save(); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to save task run store: %v", err)
	}

	return taskRunToProto(run), nil
}

func (s *taskService) GetRun(ctx context.Context, req *pb.GetRunRequest) (*pb.TaskRun, error) {
	runID := req.GetRunId()
	if runID == "" {
		return nil, status.Error(codes.InvalidArgument, "run_id is required")
	}

	run := s.server.taskRunStore.Get(runID)
	if run == nil {
		return nil, status.Error(codes.NotFound, "run not found")
	}

	return taskRunToProto(run), nil
}

func (s *taskService) ListRuns(ctx context.Context, req *pb.ListRunsRequest) (*pb.ListRunsResponse, error) {
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 50
	}

	var runs []*task.Run
	if taskID := req.GetTaskId(); taskID != "" {
		runs = s.server.taskRunStore.ListByTask(taskID, limit)
	} else {
		runs = s.server.taskRunStore.ListAll(limit)
	}

	pbRuns := make([]*pb.TaskRun, len(runs))
	for i, r := range runs {
		pbRuns[i] = taskRunToProto(r)
	}

	return &pb.ListRunsResponse{Runs: pbRuns}, nil
}

func (s *taskService) CancelRun(ctx context.Context, req *pb.CancelRunRequest) (*pb.Empty, error) {
	runID := req.GetRunId()
	if runID == "" {
		return nil, status.Error(codes.InvalidArgument, "run_id is required")
	}

	if err := s.server.taskExecutor.Cancel(runID); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to cancel run: %v", err)
	}

	if err := s.server.taskRunStore.Save(); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to save task run store: %v", err)
	}

	return &pb.Empty{}, nil
}

func (s *taskService) Export(ctx context.Context, req *pb.ExportTaskRequest) (*pb.ExportTaskResponse, error) {
	// Export not implemented in MVP
	return nil, status.Error(codes.Unimplemented, "task export not yet implemented")
}

func (s *taskService) Import(ctx context.Context, req *pb.ImportTaskRequest) (*pb.Task, error) {
	// Import not implemented in MVP
	return nil, status.Error(codes.Unimplemented, "task import not yet implemented")
}

// ============================================================================
// Helpers
// ============================================================================

func projectToProto(p *project.Project) *pb.Project {
	var lastAccessed int64
	if !p.LastAccessed.IsZero() {
		lastAccessed = p.LastAccessed.Unix()
	}

	return &pb.Project{
		Id:            p.ID,
		Name:          p.Name,
		Path:          p.Path,
		DetectedTools: p.DetectedTools,
		LastAccessed:  lastAccessed,
		IsFavorite:    p.IsFavorite,
	}
}

func sessionToProto(s *session.Session) *pb.Session {
	if s == nil {
		return nil
	}

	var pbStatus pb.SessionStatus
	switch s.Status() {
	case session.StatusRunning:
		pbStatus = pb.SessionStatus_SESSION_STATUS_RUNNING
	case session.StatusIdle:
		pbStatus = pb.SessionStatus_SESSION_STATUS_IDLE
	case session.StatusEnded:
		pbStatus = pb.SessionStatus_SESSION_STATUS_ENDED
	default:
		pbStatus = pb.SessionStatus_SESSION_STATUS_UNSPECIFIED
	}

	// Get log info from logger if available
	var logPath string
	var logSize int64
	if logger := s.GetLogger(); logger != nil {
		logPath = logger.LogPath()
		logSize = logger.BytesWritten()
	}

	return &pb.Session{
		Id:               s.ID,
		Name:             s.Name,
		ProjectId:        s.ProjectID,
		Tool:             s.Tool,
		WorkingDirectory: s.WorkingDirectory,
		Status:           pbStatus,
		CreatedAt:        s.CreatedAt.Unix(),
		LastActivity:     s.LastActivity().Unix(),
		ClientCount:      int32(s.ClientCount()),
		LogPath:          logPath,
		LogSizeBytes:     logSize,
	}
}

func taskToProto(t *task.Task) *pb.Task {
	if t == nil {
		return nil
	}

	return &pb.Task{
		Id:             t.ID,
		Name:           t.Name,
		Description:    t.Description,
		Tool:           t.Tool,
		Scope:          taskScopeToProto(t.Scope),
		ProjectId:      t.ProjectID,
		Prompt:         t.Prompt,
		Interactive:    t.Interactive,
		TimeoutSeconds: int32(t.TimeoutSeconds),
	}
}

func taskScopeToProto(scope task.TaskScope) pb.TaskScope {
	switch scope {
	case task.TaskScopeGlobal:
		return pb.TaskScope_TASK_SCOPE_GLOBAL
	case task.TaskScopeSystem:
		return pb.TaskScope_TASK_SCOPE_SYSTEM
	case task.TaskScopeProject:
		return pb.TaskScope_TASK_SCOPE_PROJECT
	default:
		return pb.TaskScope_TASK_SCOPE_UNSPECIFIED
	}
}

func protoToTaskScope(scope pb.TaskScope) task.TaskScope {
	switch scope {
	case pb.TaskScope_TASK_SCOPE_GLOBAL:
		return task.TaskScopeGlobal
	case pb.TaskScope_TASK_SCOPE_SYSTEM:
		return task.TaskScopeSystem
	case pb.TaskScope_TASK_SCOPE_PROJECT:
		return task.TaskScopeProject
	default:
		return task.TaskScopeUnspecified
	}
}

func taskRunToProto(r *task.Run) *pb.TaskRun {
	if r == nil {
		return nil
	}

	var completedAt int64
	if !r.CompletedAt.IsZero() {
		completedAt = r.CompletedAt.Unix()
	}

	return &pb.TaskRun{
		Id:          r.ID,
		TaskId:      r.TaskID,
		Status:      taskRunStatusToProto(r.Status),
		StartedAt:   r.StartedAt.Unix(),
		CompletedAt: completedAt,
		Output:      r.Output,
		Error:       r.Error,
		SessionId:   r.SessionID,
	}
}

func taskRunStatusToProto(status task.RunStatus) pb.TaskRunStatus {
	switch status {
	case task.RunStatusPending:
		return pb.TaskRunStatus_TASK_RUN_STATUS_PENDING
	case task.RunStatusRunning:
		return pb.TaskRunStatus_TASK_RUN_STATUS_RUNNING
	case task.RunStatusCompleted:
		return pb.TaskRunStatus_TASK_RUN_STATUS_COMPLETED
	case task.RunStatusFailed:
		return pb.TaskRunStatus_TASK_RUN_STATUS_FAILED
	case task.RunStatusCancelled:
		return pb.TaskRunStatus_TASK_RUN_STATUS_CANCELLED
	default:
		return pb.TaskRunStatus_TASK_RUN_STATUS_UNSPECIFIED
	}
}
