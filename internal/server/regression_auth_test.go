package server

// Regression tests from the September 2026 review: gRPC auth interceptor coverage,
// TaskService.Update semantics, ProjectService.Add path handling.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	rpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"

	"github.com/gelotto/hqsshd/internal/config"
	pb "github.com/gelotto/hqsshd/proto"
	"google.golang.org/protobuf/proto"
)

func startAuthServer(t *testing.T, token string, reflect bool) (*Server, *grpc.ClientConn) {
	t.Helper()
	srv, cfg := newTestServer(t, t.TempDir())
	cfg.AuthToken = token
	cfg.EnableReflection = reflect
	cfg.Tools = []config.ToolConfig{{Name: "claude", Command: "claude"}}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	conn, err := grpc.NewClient("unix:"+cfg.Socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return srv, conn
}

func withAuth(ctx context.Context, key, value string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, key, value)
}

func TestAuthCoversEveryRPCKind(t *testing.T) {
	_, conn := startAuthServer(t, "s3cret", true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sys := pb.NewSystemServiceClient(conn)
	sess := pb.NewSessionServiceClient(conn)

	// Unary without any metadata
	if _, err := sys.GetInfo(ctx, &pb.Empty{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("unary GetInfo without token: code=%v err=%v, want Unauthenticated", status.Code(err), err)
	}
	// Unary with wrong token
	if _, err := sys.GetInfo(withAuth(ctx, "authorization", "Bearer nope"), &pb.Empty{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("unary GetInfo wrong token: code=%v, want Unauthenticated", status.Code(err))
	}
	// Unary with correct Bearer token
	if _, err := sys.GetInfo(withAuth(ctx, "authorization", "Bearer s3cret"), &pb.Empty{}); err != nil {
		t.Errorf("unary GetInfo with Bearer token: %v", err)
	}
	// Bare token (no Bearer prefix) is also accepted by validateAuth
	if _, err := sys.GetInfo(withAuth(ctx, "authorization", "s3cret"), &pb.Empty{}); err != nil {
		t.Errorf("unary GetInfo with bare token: %v", err)
	}
	// Mixed-case metadata key: grpc-go lowercases keys, so this must work
	if _, err := sys.GetInfo(withAuth(ctx, "Authorization", "Bearer s3cret"), &pb.Empty{}); err != nil {
		t.Errorf("unary GetInfo with 'Authorization' key: %v", err)
	}
	// Lowercase "bearer " prefix is NOT stripped -> compared as a whole -> rejected
	if _, err := sys.GetInfo(withAuth(ctx, "authorization", "bearer s3cret"), &pb.Empty{}); status.Code(err) != codes.Unauthenticated {
		t.Logf("note: lowercase 'bearer ' prefix accepted? code=%v", status.Code(err))
	} else {
		t.Logf("note: lowercase 'bearer ' prefix is rejected (case-sensitive prefix strip)")
	}

	// Server-streaming WatchEvents without token
	ws, err := sess.WatchEvents(ctx, &pb.WatchEventsRequest{})
	if err == nil {
		_, err = ws.Recv()
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("server-stream WatchEvents without token: code=%v err=%v", status.Code(err), err)
	}

	// Server-streaming GetSessionLog without token
	ls, err := sess.GetSessionLog(ctx, &pb.GetSessionLogRequest{SessionId: "x"})
	if err == nil {
		_, err = ls.Recv()
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("server-stream GetSessionLog without token: code=%v err=%v", status.Code(err), err)
	}

	// Server-streaming Attach without token
	as, err := sess.Attach(ctx, &pb.AttachRequest{SessionId: "x"})
	if err == nil {
		_, err = as.Recv()
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("server-stream Attach without token: code=%v err=%v", status.Code(err), err)
	}

	// Client-streaming Input without token
	is, err := sess.Input(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = is.Send(&pb.TerminalInput{SessionId: "x", Data: []byte("x")})
	_, err = is.CloseAndRecv()
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("client-stream Input without token: code=%v err=%v", status.Code(err), err)
	}

	// Reflection (bidi stream) without token must also be rejected
	rc := rpb.NewServerReflectionClient(conn)
	rs, err := rc.ServerReflectionInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = rs.Send(&rpb.ServerReflectionRequest{MessageRequest: &rpb.ServerReflectionRequest_ListServices{ListServices: ""}})
	_, err = rs.Recv()
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("reflection without token: code=%v err=%v", status.Code(err), err)
	}
	// Reflection with token works
	rs2, err := rc.ServerReflectionInfo(withAuth(ctx, "authorization", "Bearer s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	_ = rs2.Send(&rpb.ServerReflectionRequest{MessageRequest: &rpb.ServerReflectionRequest_ListServices{ListServices: ""}})
	resp, err := rs2.Recv()
	if err != nil && err != io.EOF {
		t.Errorf("reflection with token: %v", err)
	} else if resp != nil && len(resp.GetListServicesResponse().GetService()) == 0 {
		t.Errorf("reflection returned no services")
	}
}

func TestNoAuthWhenTokenUnset(t *testing.T) {
	_, conn := startAuthServer(t, "", false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pb.NewSystemServiceClient(conn).GetInfo(ctx, &pb.Empty{}); err != nil {
		t.Errorf("GetInfo without token when auth disabled: %v", err)
	}
}

// TaskService.Update replaces every field with the request's value, so a
// partial update (only name) wipes tool/prompt/timeout/project.
func TestTaskUpdateMergesByPresence(t *testing.T) {
	srv, _ := startAuthServer(t, "", false)
	ts := &taskService{server: srv}
	ctx := context.Background()

	created, err := ts.Create(ctx, &pb.CreateTaskRequest{
		Name: "Review", Tool: "claude", Scope: pb.TaskScope_TASK_SCOPE_SYSTEM,
		Prompt: "review the code", TimeoutSeconds: 120, Description: "desc",
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := ts.Update(ctx, &pb.UpdateTaskRequest{Id: created.GetId(), Name: proto.String("Renamed")})
	if err != nil {
		t.Fatal(err)
	}
	if updated.GetName() != "Renamed" {
		t.Errorf("name not updated: %q", updated.GetName())
	}
	if updated.GetTool() != "claude" || updated.GetPrompt() != "review the code" || updated.GetTimeoutSeconds() != 120 || updated.GetDescription() != "desc" {
		t.Errorf("partial Update wiped fields: tool=%q prompt=%q timeout=%d desc=%q",
			updated.GetTool(), updated.GetPrompt(), updated.GetTimeoutSeconds(), updated.GetDescription())
	}
	// A present-but-empty text field clears it
	cleared, err := ts.Update(ctx, &pb.UpdateTaskRequest{Id: created.GetId(), Description: proto.String("")})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.GetDescription() != "" || cleared.GetPrompt() != "review the code" {
		t.Errorf("explicit empty description: desc=%q prompt=%q", cleared.GetDescription(), cleared.GetPrompt())
	}
	// PROJECT scope needs a project
	_, err = ts.Update(ctx, &pb.UpdateTaskRequest{Id: created.GetId(), Scope: pb.TaskScope_TASK_SCOPE_PROJECT.Enum()})
	if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument {
		t.Errorf("PROJECT scope without project_id: got %v, want InvalidArgument", err)
	}
	// Empty name is refused
	_, err = ts.Update(ctx, &pb.UpdateTaskRequest{Id: created.GetId(), Name: proto.String("")})
	if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument {
		t.Errorf("empty name: got %v, want InvalidArgument", err)
	}
	// An unknown tool is refused and the task is untouched
	_, err = ts.Update(ctx, &pb.UpdateTaskRequest{Id: created.GetId(), Tool: proto.String("nope")})
	if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument {
		t.Errorf("unknown tool: got %v, want InvalidArgument", err)
	}
	if got := srv.taskStore.Get(created.GetId()); got.Tool != "claude" {
		t.Errorf("failed update changed tool to %q", got.Tool)
	}
}

// ProjectService.Add normalises paths: a relative path resolves under the
// user's home (never the daemon's cwd), "~user" is refused, and trailing
// slashes do not produce a second project ID for the same directory.
func TestProjectAddPathNormalisation(t *testing.T) {
	srv, _ := startAuthServer(t, "", false)
	ps := &projectService{server: srv}
	ctx := context.Background()

	dir := t.TempDir()
	sub := filepath.Join(dir, "repo")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	t.Chdir(t.TempDir()) // the daemon's cwd must not matter

	a, err := ps.Add(ctx, &pb.AddProjectRequest{Path: "repo"})
	if err != nil {
		t.Fatalf("Add(\"repo\") relative path: %v", err)
	}
	if want, _ := filepath.EvalSymlinks(sub); a.GetPath() != sub && a.GetPath() != want {
		t.Errorf("Add(\"repo\") path = %q, want %q (under HOME)", a.GetPath(), sub)
	}
	for _, p := range []string{"~/repo", "./repo", sub} {
		got, err := ps.Add(ctx, &pb.AddProjectRequest{Path: p})
		if err != nil {
			t.Fatalf("Add(%q): %v", p, err)
		}
		if got.GetId() != a.GetId() {
			t.Errorf("Add(%q) id = %s, want %s (same directory)", p, got.GetId(), a.GetId())
		}
	}
	if _, err := ps.Add(ctx, &pb.AddProjectRequest{Path: "~root/repo"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Add(\"~root/repo\"): got %v, want InvalidArgument", err)
	}
	b, err := ps.Add(ctx, &pb.AddProjectRequest{Path: sub + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if a.GetId() != b.GetId() {
		t.Errorf("same directory registered twice: %q -> %s, %q -> %s (registry now has %d entries)",
			sub, a.GetId(), sub+"/", b.GetId(), srv.registry.Count())
	}
}

// Discover persists any client-supplied directory as a permanent scan root
// that every later List() rescans (60 s throttle, synchronously under rescanMu).
func TestDiscoverLearnsArbitraryRoot(t *testing.T) {
	srv, _ := startAuthServer(t, "", false)
	ps := &projectService{server: srv}
	dir := t.TempDir()
	if _, err := ps.Discover(context.Background(), &pb.DiscoverRequest{Directories: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	roots := srv.discoveryState.ScanRoots()
	found := false
	for _, r := range roots {
		if r == filepath.Clean(dir) {
			found = true
		}
	}
	if !found {
		t.Errorf("scan roots %v do not include %s", roots, dir)
	}
	// List runs the rescan synchronously in the RPC goroutine
	start := time.Now()
	if _, err := ps.List(context.Background(), &pb.ListProjectsRequest{}); err != nil {
		t.Fatal(err)
	}
	t.Logf("first List() (triggers synchronous rescan of %d roots) took %v", len(roots)+len(srv.config.Projects.ScanDirectories), time.Since(start))
}
