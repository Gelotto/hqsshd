package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	"github.com/gelotto/hqsshd/internal/config"
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
		fmt.Printf("Warning: failed to load project registry: %v\n", err)
	}

	// Create session manager with config values
	sessionMgr := session.NewManager(
		cfg.Sessions.IdleTimeout,
		cfg.Sessions.MaxSessions,
		cfg.Sessions.HistorySize,
		dataDir,
		cfg.Sessions.LogDirectory,
		cfg.Sessions.LogRetentionDays,
	)

	// Create task store and run store
	taskStore := task.NewStore(dataDir)
	if err := taskStore.Load(); err != nil {
		fmt.Printf("Warning: failed to load task store: %v\n", err)
	}

	taskRunStore := task.NewRunStore(dataDir)
	if err := taskRunStore.Load(); err != nil {
		fmt.Printf("Warning: failed to load task run store: %v\n", err)
	}

	// Create task executor
	taskExecutor := task.NewExecutor(taskStore, taskRunStore)

	return &Server{
		config:         cfg,
		detector:       detector,
		discovery:      discovery,
		registry:       registry,
		sessionManager: sessionMgr,
		taskStore:      taskStore,
		taskRunStore:   taskRunStore,
		taskExecutor:   taskExecutor,
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
	var tcpListener net.Listener
	if s.config.TCPPort > 0 {
		tcpAddr := fmt.Sprintf("127.0.0.1:%d", s.config.TCPPort)
		tcpListener, err = net.Listen("tcp", tcpAddr)
		if err != nil {
			unixListener.Close()
			return fmt.Errorf("failed to listen on TCP port: %w", err)
		}
	}

	// All listeners created successfully - now assign to struct
	s.unixListener = unixListener
	s.tcpListener = tcpListener

	// Create gRPC server with keepalive for detecting dead connections
	s.grpcServer = grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    20 * time.Second, // Send ping every 20s if idle
			Timeout: 5 * time.Second,  // Wait 5s for pong
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second, // Minimum time between client pings
			PermitWithoutStream: true,             // Allow pings even when no streams
		}),
	)

	// Create and register services
	systemSvc := &systemService{server: s}
	projectSvc := &projectService{server: s}
	sessionSvc := newSessionService(s)
	taskSvc := &taskService{server: s}

	pb.RegisterSystemServiceServer(s.grpcServer, systemSvc)
	pb.RegisterProjectServiceServer(s.grpcServer, projectSvc)
	pb.RegisterSessionServiceServer(s.grpcServer, sessionSvc)
	pb.RegisterTaskServiceServer(s.grpcServer, taskSvc)

	// Enable gRPC reflection for debugging with grpcurl
	reflection.Register(s.grpcServer)

	fmt.Printf("HQSSH daemon v%s starting\n", config.DaemonVersion)
	fmt.Printf("  Unix socket: %s\n", socketPath)
	if s.tcpListener != nil {
		fmt.Printf("  TCP port: 127.0.0.1:%d (for SSH tunnel)\n", s.config.TCPPort)
	}

	// Start serving on TCP listener in background goroutine
	// Note: We can't truly verify startup before Serve() accepts first connection.
	// Instead, we log errors so they're not silently lost.
	if s.tcpListener != nil {
		s.tcpWg.Add(1)
		go func() {
			defer s.tcpWg.Done()
			if err := s.grpcServer.Serve(s.tcpListener); err != nil {
				// Log error - don't silently drop it
				// Note: "use of closed network connection" is normal during shutdown
				fmt.Printf("TCP listener stopped: %v\n", err)
			}
		}()
	}

	// Start serving on Unix socket (blocks until shutdown)
	return s.grpcServer.Serve(s.unixListener)
}

// Stop gracefully stops the server
func (s *Server) Stop() {
	// Close task executor first (cancels all running tasks)
	if s.taskExecutor != nil {
		s.taskExecutor.Close()
	}

	// Close session manager (kills all sessions)
	if s.sessionManager != nil {
		s.sessionManager.Close()
	}

	// Gracefully stop gRPC server (stops both listeners)
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
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
			fmt.Printf("Warning: failed to save project registry: %v\n", err)
		}
	}

	// Save task store
	if s.taskStore != nil {
		if err := s.taskStore.Save(); err != nil {
			fmt.Printf("Warning: failed to save task store: %v\n", err)
		}
	}

	// Save task run store
	if s.taskRunStore != nil {
		if err := s.taskRunStore.Save(); err != nil {
			fmt.Printf("Warning: failed to save task run store: %v\n", err)
		}
	}
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
		fmt.Printf("Warning: failed to save registry: %v\n", err)
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
		fmt.Printf("Warning: failed to save registry: %v\n", err)
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
		fmt.Printf("Warning: failed to save registry: %v\n", err)
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

	// Default terminal size
	cols := int(req.GetCols())
	rows := int(req.GetRows())
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	// Get optional tool arguments
	args := req.GetArgs()

	// Create session
	sess, err := s.server.sessionManager.Create(projectID, tool, workingDir, args, cols, rows)
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

	// Attach to session
	clientID, outputCh, scrollback, err := s.server.sessionManager.Attach(sessionID, cols, rows)
	if err != nil {
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
	// Note: Detach is handled automatically when Attach stream ends
	// This RPC is for explicit detach requests
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
		fmt.Printf("Warning: failed to save task store: %v\n", err)
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
		fmt.Printf("Warning: failed to save task store: %v\n", err)
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
		fmt.Printf("Warning: failed to save task store: %v\n", err)
	}
	if err := s.server.taskRunStore.Save(); err != nil {
		fmt.Printf("Warning: failed to save task run store: %v\n", err)
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
		fmt.Printf("Warning: failed to save task run store: %v\n", err)
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
		fmt.Printf("Warning: failed to save task run store: %v\n", err)
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
